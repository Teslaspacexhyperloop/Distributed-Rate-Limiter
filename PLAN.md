# Distributed API Gateway & Rate Limiter — Build Plan

## Architecture Summary

```
Client → NGINX → [Gateway 1 | Gateway 2] → Redis (shared state) → [User/Product/Order Services]
```

The core distributed systems question: **how do two stateless gateway instances enforce one atomic rate limit?** Every phase builds toward answering that.

---

## Phase 1 — Gateway MVP (2–3 days)

### What you build
- Go project with Chi router
- Reverse proxy using `net/http/httputil.ReverseProxy`
- Three mock backend services (User, Product, Order)
- Request ID middleware and structured logging with `slog`
- Basic Docker Compose with all services

### File targets
```
cmd/gateway/main.go
cmd/user-service/main.go
cmd/product-service/main.go
cmd/order-service/main.go
internal/proxy/reverse_proxy.go
internal/routing/routes.go
internal/middleware/request_id.go
internal/middleware/logging.go
internal/config/config.go
docker-compose.yml
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Stateless design from day 1** | Every decision you make here either supports or breaks horizontal scaling later. If you store anything in process memory (route counts, sessions, etc.), Phase 4 breaks. |
| **Request ID propagation** | In a distributed system, a single logical request touches NGINX, a gateway instance, and a backend. Without a correlation ID flowing through all hops, debugging is impossible. |
| **Timeout configuration** | If a backend hangs, your goroutine hangs. Multiply that by 100 concurrent requests and the gateway is deadlocked. Set `http.Client` timeouts at the transport layer, not the handler level. |
| **Header forwarding** | `ReverseProxy` copies most headers but strips `X-Forwarded-For` unless you configure it. Backends need the real client IP for their own logic. |

**Success check:** `curl http://localhost:8080/api/products` returns mock JSON through the gateway.

---

## Phase 2 — Redis Rate Limiter (4–5 days)

### What you build
- Token Bucket algorithm backed by Redis
- Atomic Lua script for check-and-decrement
- Rate-limit middleware with `X-RateLimit-*` headers
- HTTP 429 responses with retry-after duration
- Redis key format: `rate-limit:{userId}:{route}`

### File targets
```
internal/ratelimiter/limiter.go
internal/ratelimiter/redis.go
internal/ratelimiter/token_bucket.lua
```

### The Lua script — the hardest part of this phase

```lua
-- What it must do atomically:
-- 1. Read current tokens + last refill timestamp
-- 2. Compute elapsed time → calculate tokens to add
-- 3. Cap at bucket max capacity
-- 4. If tokens >= 1: deduct 1, save state, return ALLOWED
-- 5. Else: return REJECTED + retry-after
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Race condition without atomicity** | Two gateway instances both read `tokens=1`. Both see "allowed." Both decrement. You've served 2 requests against a limit of 1. This is the classic **TOCTOU** (time-of-check to time-of-use) bug. Redis Lua scripts execute as a single transaction — no other command can interleave. |
| **Clock drift across instances** | Token refill is time-based. If Gateway 1 and Gateway 2 use their local clocks in the Lua script, refill calculations diverge. **Solution:** use `redis.call('TIME')` inside Lua — the timestamp comes from the Redis server, not the gateway. |
| **Key expiration strategy** | A rate-limit key for a user who never comes back should not live in Redis forever. Set TTL = window size + small buffer. Too short = state loss mid-window. Too long = memory leak. |
| **Fail-open vs fail-closed** | Redis goes down. Do you block all traffic (fail-closed, safe but disruptive) or allow all traffic (fail-open, available but bypassable)? This is a **CAP theorem trade-off** — you must pick consistency or availability. Implement `RATE_LIMIT_FAILURE_MODE` env var. |
| **Token refill math** | Continuous vs discrete refill. A sliding window refills gradually; a fixed window refills all at once (burst vulnerability). Token Bucket is a middle path — you must implement the refill formula correctly: `tokens = min(capacity, stored_tokens + (elapsed_seconds * rate))` |

**Success check:** Send 101 requests. First 100 return 200, 101st returns 429 with correct headers.

---

## Phase 3 — JWT Authentication (2–3 days)

### What you build
- `POST /auth/register` and `POST /auth/login` endpoints
- JWT generation with `sub` (user ID) and `plan` claims
- JWT validation middleware
- Plan-based rate limits (free=100/min, premium=1000/min)
- Public vs protected route separation

### File targets
```
internal/auth/jwt.go
internal/auth/middleware.go
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Stateless authentication** | JWTs are self-contained — the gateway validates the token locally using a shared secret, with zero database calls. This is what makes horizontal scaling possible. If you used server-side sessions, every gateway instance would need session-store access. |
| **Secret sharing across instances** | All gateway instances must use the same `JWT_SECRET`. In Docker Compose this is an env var. In production this would be Vault or Kubernetes Secrets. Hardcoding it makes it impossible to rotate. |
| **Token expiration and clock skew** | JWT `exp` is a Unix timestamp. If your gateway server clock is behind by a few seconds, a just-expired token is still accepted. Libraries add a small leeway; understand why. |
| **Never log tokens** | A JWT in logs = a replayable credential in your log aggregation system. Strip `Authorization` headers before passing to `slog`. |
| **Plan extraction → rate-limit key routing** | The middleware must extract the plan claim and pass it downstream so the rate limiter can apply the correct bucket size. Use Go's `context.Context` to carry authenticated claims — don't re-parse the token in the rate limiter. |

**Success check:** Free token gets 429 at request 101. Premium token handles 1000 requests.

---

## Phase 4 — Distributed Deployment (2–3 days)

### What you build
- Two gateway instances in Docker Compose (`gateway1`, `gateway2`)
- NGINX load balancer in round-robin across both instances
- Shared Redis between instances
- Multi-gateway integration test proving shared state

### File targets
```
nginx/nginx.conf
docker-compose.yml  (updated)
tests/integration/multi_gateway_test.go
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Proving shared state** | This is the thesis of the whole project. Run 60 requests to Gateway 1 and 40 requests to Gateway 2. The 101st request to either must return 429. If you get this wrong, your rate limiter is per-instance (broken). |
| **NGINX as a single point of failure** | You've distributed the gateways but NGINX itself is now the SPOF. In production this is solved with DNS-based load balancing or an anycast IP. Worth discussing in the README even if not implemented. |
| **Instance identity in metrics** | Both gateways emit metrics. Without a `gateway_instance` label, Prometheus mixes them. Add the hostname or a `GATEWAY_ID` env var to every metric label — otherwise your Grafana dashboards will show aggregated data that hides per-instance problems. |
| **Graceful shutdown coordination** | When Docker Compose restarts Gateway 1, in-flight requests should complete. NGINX should stop sending new requests to Gateway 1 before it shuts down. This requires both `SIGTERM` handling in Go and appropriate NGINX upstream health checks. |
| **State durability** | If Redis restarts, all token buckets reset. Users get a free window of requests. This is acceptable for rate limiting but must be documented as a known behavior. A Redis persistence strategy (AOF or RDB snapshots) changes this — discuss the trade-off. |

**Success check:** 60 requests to `:8081` + 40 requests to `:8082` = 429 on request 101 to either port.

---

## Phase 5 — Reliability (2–3 days)

### What you build
- Request timeouts and graceful shutdown
- Circuit breaker (`sony/gobreaker`) per backend service
- Retry with exponential backoff for 502/503/504
- Redis fail-open and fail-closed modes

### File targets
```
internal/resilience/circuit_breaker.go
internal/resilience/retry.go
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Circuit breaker state machine** | CLOSED → OPEN → HALF-OPEN. When Product Service is down, every request waits for a timeout before failing. With a circuit breaker, after N failures the breaker opens and requests fail instantly — protecting your thread pool and giving the backend recovery time. |
| **Retry amplification** | If Product Service is slow and you retry 3 times, you've tripled your load on an already struggling service. Exponential backoff reduces this, but you must also implement **jitter** to prevent synchronized retries from N gateway goroutines all hitting the backend at the same time (thundering herd). |
| **Idempotency constraint** | Only retry GET, HEAD, PUT. Never auto-retry POST or DELETE — you don't know if the first attempt reached the backend and partially executed. The requirements limit retries to network failures and 5xx, which is correct, but you should understand why POST retries are dangerous. |
| **Graceful shutdown** | `http.Server.Shutdown()` stops accepting new connections but waits for in-flight requests to finish. You must also drain the reverse proxy's outbound connections. Use `signal.NotifyContext` to catch SIGTERM and trigger a shutdown with a deadline. |
| **Cascading failures** | Without circuit breakers, one slow backend can exhaust your gateway's goroutine pool. Demonstrate this: turn off Product Service, show gateway latency rising for `/api/users` requests even though User Service is healthy. Then add a circuit breaker and show isolation. |

---

## Phase 6 — Observability & Benchmarking (3–4 days)

### What you build
- Prometheus metrics exposed at `GET /metrics`
- Grafana dashboard with panels for latency, throughput, error rate
- k6 load tests: normal, spike, stress, rate-limit
- Real benchmark numbers in README

### File targets
```
internal/middleware/metrics.go
monitoring/prometheus.yml
monitoring/grafana/dashboards/gateway.json
load-tests/normal-load.js
load-tests/spike-test.js
load-tests/stress-test.js
load-tests/rate-limit-test.js
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Cardinality explosion** | If you label metrics with user ID (`user_123`, `user_456`…), you create millions of unique time series in Prometheus and crash it. Use only low-cardinality labels: route, method, status code, gateway instance. |
| **Latency histogram buckets** | The default Prometheus histogram buckets (`.005`, `.01`, `.025`…) are designed for web apps. For a gateway that should respond in <5ms, you need custom buckets. Otherwise p99 reads as "under 25ms" which is useless. |
| **Interpreting p99 vs p95 under load** | Under k6 spike test, you'll see p99 spike while p50 stays flat. This reveals tail-latency caused by goroutine scheduling or Redis connection pool exhaustion — not visible in averages. This is the real value of percentile histograms. |
| **Redis connection pool under load** | At 200 concurrent virtual users, if your Redis client has a pool size of 10, you'll see Redis operation latency spike. The fix is tuning `PoolSize` in `go-redis`. The Grafana `redis_operation_duration_seconds` metric will show this clearly. |
| **Load test as distributed systems proof** | The `rate-limit-test.js` test is the final demonstration: send 150 requests for a user with a 100/min limit, distributed across both gateways. The k6 output must show exactly 100 status=200 and 50 status=429. Any other split means your distributed locking is broken. |

---

## Execution Order Within Each Phase

For each phase, always build in this sequence:

1. **Core logic first** (pure Go, no external deps) — write unit tests here
2. **Redis/external integration** — write integration tests with Testcontainers
3. **Middleware wiring** — plug into the Chi router
4. **Docker Compose update** — verify with `docker compose up`
5. **Failure scenario** — kill a dependency and verify the error path

---

## Key Distributed Systems Concepts Demonstrated

| Concept | Where demonstrated |
|---|---|
| Atomic distributed locking | Redis Lua script (Phase 2) |
| TOCTOU race condition prevention | Concurrency test: 100 goroutines, limit=50 (Phase 2) |
| Stateless horizontal scaling | Two gateways sharing Redis state (Phase 4) |
| CAP theorem trade-off | Fail-open vs fail-closed Redis mode (Phase 2/5) |
| Cascading failure isolation | Circuit breaker per backend (Phase 5) |
| Thundering herd prevention | Exponential backoff with jitter (Phase 5) |
| Tail latency measurement | p95/p99 Prometheus histograms under k6 load (Phase 6) |
| Clock synchronization | Redis-side `TIME` in Lua for token refill (Phase 2) |
| Correlation across services | Request ID propagated through all hops (Phase 1) |

---

## README Checklist

The README is the portfolio artifact. Include:

- Architecture diagram
- How the Lua script prevents the race condition (with the broken naive version as contrast)
- Real k6 numbers (not invented — run the tests first)
- A "what breaks without Redis" section showing the per-instance counter bug
- Docker Compose one-liner to spin the whole thing up

The project answers one specific, hard question — **how do stateless services enforce one shared invariant without locks in application code?** — and every phase is scaffolding toward that answer.
