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

## Phase 2 — Multi-Algorithm Redis Rate Limiter (5–7 days)

### What you build

Implement **four rate-limiting algorithms** all backed by Redis Lua scripts. Each is a separate, swappable strategy behind a common `Limiter` interface. The algorithm is selected per-request via config or JWT plan claim.

#### Algorithms

| Algorithm | Key characteristic | Best for |
|---|---|---|
| **Token Bucket** | Smooth refill, burst-tolerant | APIs that need occasional bursts |
| **Sliding Window** | Exact rolling window, no burst | APIs requiring strict per-second limits |
| **Fixed Window** | Resets on interval boundary | Simple, memory-efficient global caps |
| **Leaky Bucket** | Constant drain rate, queued | Downstream protection, traffic shaping |

#### Algorithm comparison (important for learning)

```
Token Bucket:    [||||||||  ] 80/100 tokens, burst of 20 allowed
Sliding Window:  tracks exact timestamps, no burst allowed ever
Fixed Window:    resets at :00 — 100 allowed, then 0 until next window
Leaky Bucket:    regardless of burst, drain rate is always constant
```

The classic attack on **Fixed Window**: send 100 requests at 11:59:59 and 100 at 12:00:00 — you get 200 requests across the boundary. Token Bucket and Sliding Window both prevent this.

### File targets
```
internal/ratelimiter/limiter.go          (Limiter interface)
internal/ratelimiter/token_bucket.go
internal/ratelimiter/sliding_window.go
internal/ratelimiter/fixed_window.go
internal/ratelimiter/leaky_bucket.go
internal/ratelimiter/redis.go
internal/ratelimiter/scripts/token_bucket.lua
internal/ratelimiter/scripts/sliding_window.lua
internal/ratelimiter/scripts/fixed_window.lua
internal/ratelimiter/scripts/leaky_bucket.lua
```

### Hierarchical configuration (from reference repo pattern)

Rate-limit config resolves in priority order — highest specificity wins:

```
1. Per-key config:      rate-limit:user_123:/api/products   → 500 req/min
2. Pattern config:      rate-limit:user_*:/api/*             → 200 req/min
3. Plan-based default:  plan=free                            → 100 req/min
4. Global default:                                           → 50 req/min
```

```
internal/ratelimiter/config.go     (hierarchical resolver)
```

### Redis key format
```
rate-limit:{userId}:{route}           per-user per-route
rate-limit:pattern:{glob}             pattern match cache
rate-limit:global:{route}             global per-route cap
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Race condition without atomicity** | Two gateway instances both read `tokens=1`. Both see "allowed." Both decrement. Serving 2 requests against a limit of 1. This is the classic **TOCTOU** bug. Redis Lua executes atomically — no interleave possible. |
| **Clock drift across instances** | Token refill is time-based. Use `redis.call('TIME')` inside Lua — the timestamp is from the Redis server, not individual gateways. |
| **Fixed window boundary burst** | Send 100 req at 11:59:59 and 100 at 12:00:01. Fixed Window allows 200 in 2 seconds. Sliding Window and Token Bucket prevent this — demonstrate it with a test. |
| **Leaky Bucket queue mechanics** | Unlike Token Bucket which rejects, Leaky Bucket can queue requests and process at constant rate. Choosing between reject vs queue is an explicit design decision with different failure modes. |
| **Key expiration strategy** | TTL = window size + buffer. Too short = state loss mid-window. Too long = memory leak. Each algorithm needs a different TTL strategy. |
| **Hierarchical config resolution** | Pattern matching across thousands of keys must be fast. Cache resolved configs in Redis with their own TTL — avoid scanning every request. |
| **Fail-open vs fail-closed** | Redis goes down. Block all traffic (fail-closed, safe) or allow all (fail-open, available)? This is a **CAP theorem trade-off**. Implement `RATE_LIMIT_FAILURE_MODE` env var. |

**Success check:** Send 101 requests. First 100 return 200, 101st returns 429. Run same test against all four algorithms and show different behavior at window boundaries.

---

## Phase 3 — JWT Authentication + Admin API + IP Security (3–4 days)

### What you build

#### 3a — JWT authentication
- `POST /auth/register` and `POST /auth/login` endpoints
- JWT generation with `sub` (user ID), `plan`, and `algorithm` claims
- JWT validation middleware
- Plan-based rate limits passed to the hierarchical config resolver

#### 3b — Admin API (dynamic config without restart)
```
GET    /admin/keys                   list all active rate-limit keys + stats
GET    /admin/limits/{key}           get current limit config for a key
PUT    /admin/limits/{key}           override limit for a specific key at runtime
DELETE /admin/limits/{key}           remove key override, fall back to plan default
POST   /admin/config/reload          flush config cache, re-read env vars
GET    /admin/config/stats           total keys, memory usage, algorithm distribution
```

#### 3c — IP whitelist/blacklist
- Configurable CIDR ranges via env vars
- Whitelisted IPs bypass rate limiting entirely
- Blacklisted IPs are rejected at the gateway before any processing

```
RATE_LIMIT_IP_WHITELIST=192.168.1.0/24,10.0.0.0/8
RATE_LIMIT_IP_BLACKLIST=1.2.3.4
```

### File targets
```
internal/auth/jwt.go
internal/auth/middleware.go
internal/admin/handlers.go
internal/admin/routes.go
internal/security/ip_filter.go
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Stateless authentication** | JWTs are self-contained — validated locally using a shared secret, zero database calls. Server-side sessions would require every gateway instance to share session storage. |
| **Secret sharing across instances** | All gateways must share `JWT_SECRET`. In Docker Compose this is an env var; in production, Vault or Kubernetes Secrets. Hardcoded secrets cannot be rotated. |
| **Admin API consistency** | When you `PUT /admin/limits/user_123` on Gateway 1, Gateway 2 must see the change immediately. Admin writes go directly to Redis; gateways read from Redis on every request. Config cache TTL is the propagation delay window. |
| **Cache invalidation** | `POST /admin/config/reload` must flush the local config cache on the receiving gateway. But the other gateway instance still has its cache. Use Redis pub/sub to broadcast cache-flush events to all instances. |
| **Never log tokens** | A JWT in logs = a replayable credential in your log aggregation system. Strip `Authorization` headers before passing to `slog`. |

**Success check:** `PUT /admin/limits/user_123` changes the rate from 100 to 500 req/min with zero restart. Verify on both gateway instances.

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
| **Proving shared state** | Run 60 requests to Gateway 1 and 40 to Gateway 2. The 101st request to either must return 429. If this fails, your rate limiter is per-instance (broken). This is the thesis test of the whole project. |
| **NGINX as a single point of failure** | You've distributed the gateways but NGINX is now the SPOF. In production: DNS-based load balancing or anycast IP. Document this in the README. |
| **Instance identity in metrics** | Both gateways emit metrics. Without a `gateway_instance` label, Prometheus merges them. Add hostname or `GATEWAY_ID` env var to every metric. |
| **Admin API + pub/sub broadcast** | A config update to one gateway must propagate to the other. Use Redis pub/sub: admin write → publish event → all subscribers flush their local cache. |
| **Graceful shutdown coordination** | NGINX must stop sending to Gateway 1 before it shuts down. Requires `SIGTERM` handling in Go + NGINX upstream health checks. |
| **State durability** | Redis restart resets all buckets — users get a free window. Acceptable for rate limiting; document it. Redis AOF persistence changes this; discuss the trade-off. |

**Success check:** 60 + 40 requests across two gateways = 429 on request 101 to either instance.

---

## Phase 5 — Reliability (2–3 days)

### What you build
- Request timeouts and graceful shutdown
- Circuit breaker (`sony/gobreaker`) per backend service
- Retry with exponential backoff + jitter for 502/503/504
- Redis fail-open and fail-closed modes

### File targets
```
internal/resilience/circuit_breaker.go
internal/resilience/retry.go
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Circuit breaker state machine** | CLOSED → OPEN → HALF-OPEN. Without it, a slow backend ties up goroutines for the full timeout duration — multiplied across concurrent requests, the gateway deadlocks. The breaker fails fast and protects the thread pool. |
| **Retry amplification** | Retrying 3× against a struggling service triples its load. Exponential backoff reduces rate; **jitter** prevents N goroutines from all retrying at the same moment (thundering herd). |
| **Idempotency constraint** | Only retry GET, HEAD, PUT. Never auto-retry POST/DELETE — the first attempt may have partially executed on the backend. |
| **Cascading failures** | One slow service without isolation will exhaust the gateway's goroutine pool and degrade completely unrelated routes. Demonstrate: kill Product Service → `/api/users` latency rises without circuit breaker, stays normal with it. |
| **Graceful shutdown** | `http.Server.Shutdown()` + `signal.NotifyContext` for SIGTERM. Drain in-flight requests before exit. |

---

## Phase 6 — Observability & Benchmarking (3–4 days)

### What you build
- Prometheus metrics at `GET /metrics`
- Grafana dashboard: latency, throughput, algorithm distribution, error rate
- Performance baseline tracking — store benchmark runs, compare regressions
- k6 load tests: normal, spike, stress, rate-limit, algorithm-comparison
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
load-tests/algorithm-comparison.js
load-tests/baseline.json              (stored benchmark results)
```

### Prometheus metrics
```
gateway_requests_total{route, method, status_code, gateway_instance}
gateway_request_duration_seconds{route, method}
rate_limit_allowed_total{algorithm, plan, route}
rate_limit_rejected_total{algorithm, plan, route}
redis_operation_duration_seconds{operation, algorithm}
backend_request_failures_total{service}
active_requests{gateway_instance}
circuit_breaker_state{service}        open=1, closed=0
algorithm_distribution{algorithm}     which algorithm is handling which share of traffic
```

### Grafana panels
- Requests per second (by gateway instance)
- p50 / p95 / p99 latency
- Allowed vs rejected (by algorithm — Token Bucket vs Sliding Window rejection curves differ)
- Algorithm distribution across traffic
- Redis operation latency histogram
- Circuit breaker state timeline
- Active connections per gateway

### Performance baseline tracking
Store k6 run results in `load-tests/baseline.json`. On each new run, compare:
- Throughput regression (>5% drop = flag)
- p99 regression (>10% increase = flag)

This catches performance regressions introduced when adding new features.

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Cardinality explosion** | Labeling metrics with user IDs creates millions of unique time series and crashes Prometheus. Use only low-cardinality labels: route, algorithm, plan, status code, gateway instance. |
| **Algorithm comparison under load** | Under identical k6 spike load, Token Bucket and Sliding Window produce different rejection curves. Token Bucket absorbs the initial burst then rejects; Sliding Window rejects immediately once the window fills. Grafana makes this visible. |
| **Latency histogram buckets** | Default Prometheus buckets (5ms–10s) are too coarse for a gateway that should respond in <2ms. Use custom buckets: `[0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1]`. |
| **Redis pool exhaustion** | At 200 concurrent VUs, a pool size of 10 creates a queue. `redis_operation_duration_seconds` will show this as latency spikes. Tune `PoolSize` in `go-redis` and benchmark the difference. |
| **Load test as distributed systems proof** | `rate-limit-test.js`: send 150 requests across both gateways for a user with a 100/min limit. k6 must show exactly 100 × 200 and 50 × 429. Any other split means distributed locking is broken. |

---

## Phase 7 — Advanced: Composite Limits, Geo Rate Limiting & Batch API (4–5 days)

This phase adds production-grade features that significantly expand the distributed systems scope.

### 7a — Composite Rate Limiting

Enforce multiple simultaneous limits with configurable combination logic. Useful for enterprise-tier plans where a user has both a per-second burst cap and a per-day quota.

```json
{
  "key": "enterprise:user_123",
  "compositeConfig": {
    "limits": [
      { "name": "burst",     "algorithm": "TOKEN_BUCKET",    "capacity": 50,    "refillRate": 10  },
      { "name": "daily",     "algorithm": "FIXED_WINDOW",    "capacity": 10000, "windowSecs": 86400 },
      { "name": "sustained", "algorithm": "SLIDING_WINDOW",  "capacity": 200,   "windowSecs": 60  }
    ],
    "combinationLogic": "ALL_MUST_PASS"
  }
}
```

**Combination logics:**
- `ALL_MUST_PASS` — every limit must allow (AND logic, strictest)
- `ANY_CAN_PASS` — at least one limit permits (OR logic, most permissive)
- `HIERARCHICAL_AND` — parent limit checked first, child limits only if parent allows
- `PRIORITY_BASED` — limits evaluated in priority order, first rejecting wins

```
internal/ratelimiter/composite.go
```

### 7b — Geographic Rate Limiting

Apply different limits based on client location detected from CDN headers. Useful for compliance (GDPR tighter limits on EU traffic) and capacity management (stricter limits for regions with known abuse).

**Supported CDN headers:**
- CloudFlare: `CF-IPCountry`, `CF-IPContinent`
- AWS CloudFront: `CloudFront-Viewer-Country`
- Azure CDN: `X-Azure-ClientIP` + GeoIP lookup

**Compliance zone mapping:**
```
DE, FR, IT, ES, NL → GDPR zone → 500 req/min cap
CA               → PIPEDA zone → 800 req/min cap
US-CA            → CCPA zone  → 800 req/min cap
*                → default    → plan-based limit
```

**Endpoints:**
```
POST /api/ratelimit/geo/rules        create or update a geographic rule
GET  /api/ratelimit/geo/rules        list all rules
GET  /api/ratelimit/geo/detect       show detected location for current request
GET  /api/ratelimit/geo/stats        allowed/rejected counts by zone
```

```
internal/ratelimiter/geo.go
internal/ratelimiter/geo_rules.go
```

### 7c — Batch Check API

Check rate limits for multiple keys in a single round-trip to the gateway. Essential for service-to-service use cases where a single action needs to validate multiple limits before proceeding.

```
POST /api/ratelimit/batch

Request:
{
  "checks": [
    { "key": "user_123", "route": "/api/products", "tokens": 1 },
    { "key": "user_123", "route": "/api/orders",   "tokens": 1 }
  ]
}

Response:
{
  "results": [
    { "key": "user_123:/api/products", "allowed": true,  "remaining": 45 },
    { "key": "user_123:/api/orders",   "allowed": false, "retryAfter": 12 }
  ],
  "allAllowed": false
}
```

### File targets
```
internal/ratelimiter/composite.go
internal/ratelimiter/geo.go
internal/ratelimiter/batch.go
internal/admin/geo_handlers.go
```

### Distributed systems challenges

| Challenge | Why it matters |
|---|---|
| **Composite atomicity** | Each component limit is a separate Redis key. Checking all three and then allowing the request is NOT atomic — a concurrent request can slip through between checks. Use a single Lua script that evaluates all composite limits in one atomic transaction. |
| **Geo detection reliability** | CDN headers can be spoofed or absent (direct connections, internal traffic). Your geo limiter must have an explicit fallback chain and must log when detection fails. Fail to default plan, not to unrestricted. |
| **Batch partial failure** | In a batch of 5 checks, check 3 fails due to Redis error. Do you fail the entire batch or return partial results? Either is defensible — document and implement the chosen behavior. |
| **Composite limit fairness** | With `ALL_MUST_PASS`, the strictest limit always wins. A user who is under their daily quota but over their burst cap gets rejected. The response must indicate WHICH limit rejected — not just "rejected." |
| **Geographic rule consistency** | Geographic rules are stored in Redis. When you add a new rule, both gateway instances must see it. Same cache invalidation problem as the Admin API — use Redis pub/sub. |

---

## Execution Order Within Each Phase

For each phase, build in this sequence:

1. **Core logic first** (pure Go, no external deps) — unit tests here
2. **Redis/external integration** — integration tests with Testcontainers
3. **Middleware wiring** — plug into Chi router
4. **Docker Compose update** — verify with `docker compose up`
5. **Failure scenario** — kill a dependency, verify the error path

---

## Full Project Structure

```
distributed-api-gateway/
│
├── cmd/
│   ├── gateway/main.go
│   ├── user-service/main.go
│   ├── product-service/main.go
│   └── order-service/main.go
│
├── internal/
│   ├── auth/
│   │   ├── jwt.go
│   │   └── middleware.go
│   ├── ratelimiter/
│   │   ├── limiter.go              (interface)
│   │   ├── token_bucket.go
│   │   ├── sliding_window.go
│   │   ├── fixed_window.go
│   │   ├── leaky_bucket.go
│   │   ├── composite.go
│   │   ├── geo.go
│   │   ├── batch.go
│   │   ├── config.go               (hierarchical resolver)
│   │   ├── redis.go
│   │   └── scripts/
│   │       ├── token_bucket.lua
│   │       ├── sliding_window.lua
│   │       ├── fixed_window.lua
│   │       └── leaky_bucket.lua
│   ├── proxy/reverse_proxy.go
│   ├── routing/routes.go
│   ├── resilience/
│   │   ├── circuit_breaker.go
│   │   └── retry.go
│   ├── middleware/
│   │   ├── request_id.go
│   │   ├── logging.go
│   │   └── metrics.go
│   ├── admin/
│   │   ├── handlers.go
│   │   ├── geo_handlers.go
│   │   └── routes.go
│   ├── security/
│   │   └── ip_filter.go
│   └── config/config.go
│
├── tests/
│   ├── integration/
│   └── concurrency/
│
├── load-tests/
│   ├── normal-load.js
│   ├── spike-test.js
│   ├── stress-test.js
│   ├── rate-limit-test.js
│   ├── algorithm-comparison.js
│   └── baseline.json
│
├── monitoring/
│   ├── prometheus.yml
│   └── grafana/dashboards/
│
├── nginx/nginx.conf
├── Dockerfile
├── docker-compose.yml
├── go.mod
└── README.md
```

---

## Key Distributed Systems Concepts Demonstrated

| Concept | Phase | Where demonstrated |
|---|---|---|
| Atomic distributed locking | 2 | Redis Lua script prevents TOCTOU across gateway instances |
| Algorithm trade-offs | 2 | Fixed window boundary burst vs Token Bucket vs Sliding Window |
| Hierarchical config resolution | 2 | Per-key → pattern → plan → global fallback chain |
| Stateless authentication | 3 | JWT validated locally, no session store needed |
| Dynamic config propagation | 3/4 | Admin API writes to Redis, pub/sub invalidates all caches |
| CAP theorem trade-off | 2/5 | Fail-open (availability) vs fail-closed (consistency) |
| Stateless horizontal scaling | 4 | Two gateways share one Redis state |
| SPOF identification | 4 | NGINX as load balancer — discuss mitigation |
| Cascading failure isolation | 5 | Circuit breaker prevents one bad service killing the gateway |
| Thundering herd prevention | 5 | Exponential backoff with jitter on retries |
| Cardinality in observability | 6 | Low-cardinality Prometheus labels vs per-user label explosion |
| Tail latency measurement | 6 | p95/p99 reveals pool exhaustion invisible to averages |
| Composite atomicity | 7 | Multi-limit check in single Lua transaction |
| Geo compliance enforcement | 7 | CDN header detection + per-zone limit rules |
| Clock synchronization | 2 | `redis.call('TIME')` for refill — not gateway local clock |
| Partial failure semantics | 7 | Batch check: what does "partial Redis failure" mean? |

---

## README Checklist

- Architecture diagram with all phases labeled
- Side-by-side algorithm comparison with the window-boundary burst attack demonstrated
- How the Lua script prevents the race condition (show the broken naive version as contrast)
- Real k6 numbers (run the tests — do not invent numbers)
- A "what breaks without Redis" section showing per-instance counter divergence
- Admin API demo: change a limit at runtime, verify on both gateway instances
- Docker Compose one-liner to spin the full system
- Performance baseline table from `load-tests/baseline.json`

The project answers one specific, hard question — **how do stateless services enforce one shared invariant without locks in application code?** — and every phase is scaffolding toward that answer.
