# Distributed API Gateway & Rate Limiter

A production-grade API gateway written in Go that answers one specific distributed systems question: **how do stateless services enforce one shared invariant — a rate limit — without locks in application code?**

The answer: atomic Redis Lua scripts, a shared-state design, and per-instance circuit breakers that isolate failure.

---

## Architecture

```
Client
  │
  ▼
NGINX :8090          ← round-robin load balancer; blocks admin API
  │
  ├── Gateway 1 :8091  ┐
  └── Gateway 2 :8092  ┘  ← two stateless instances, shared Redis
          │
          ▼
      Redis :6380      ← rate-limit buckets, user store, config overrides
          │
  ┌───────┼───────┐
  ▼       ▼       ▼
User   Product  Order
:8081  :8082    :8083
```

Each gateway instance is stateless. All mutable state lives in Redis:

| State | Redis key pattern |
|---|---|
| Rate-limit counters | `rate-limit:{userId}:{route}` |
| Per-key config overrides | `rl-config:rate-limit:{userId}:{route}` |
| User accounts | `user:{username}` |

---

## Features

### Four Rate-Limiting Algorithms

All backed by atomic Redis Lua scripts — the Lua execution model prevents the TOCTOU race where two gateway instances both read "1 token left" and both allow the request.

| Algorithm | Characteristic | Best for |
|---|---|---|
| **Token Bucket** | Smooth refill, burst-tolerant | APIs needing occasional bursts |
| **Sliding Window** | Exact rolling count, no burst | Strict per-minute limits |
| **Fixed Window** | Resets at interval boundary | Simple per-hour caps |
| **Leaky Bucket** | Constant drain rate | Downstream traffic shaping |

**Why not Fixed Window alone?** Send 100 requests at 11:59:59 and 100 at 12:00:01 — Fixed Window allows 200 in 2 seconds. Sliding Window and Token Bucket prevent this boundary burst.

Algorithm is selected per-user via the JWT claim — Alice can use `SLIDING_WINDOW` while Bob uses `TOKEN_BUCKET`, configured at registration.

### Hierarchical Rate Limit Resolution

Config resolves from most-specific to least-specific:

```
1. Per-key Redis override     (admin API, runtime, immediate effect)
2. Plan + algorithm from JWT  (pro plan + SLIDING_WINDOW = 500 req/min)
3. Plan default               (free = 100, pro = 500, enterprise = 2000)
4. Global fallback            (50 req/min)
```

### JWT Authentication

- `POST /auth/register` — bcrypt password storage in Redis; JWT returned with `sub` (user UUID), `plan`, and `algorithm` claims
- `POST /auth/login` — bcrypt verify; fresh JWT issued
- JWT validated locally on each gateway (shared `JWT_SECRET`) — zero database calls per request
- Token from Gateway 1 accepted by Gateway 2; user registered on Gateway 1 found by Gateway 2 — shared Redis

### IP Security

- **Whitelist** (CIDR ranges) — matching IPs bypass rate limiting entirely; useful for internal services and health-check probes
- **Blacklist** (CIDR ranges) — matching IPs receive `403` before any processing

Evaluated before JWT validation so blacklisted IPs never touch auth logic.

### Admin API (Runtime Config)

All config changes take effect immediately via Redis pub/sub cache invalidation — no gateway restart needed.

| Method | Endpoint | Action |
|---|---|---|
| `GET` | `/admin/keys` | List all active rate-limit counter keys |
| `GET` | `/admin/limits/{key}` | Read current config for a key |
| `PUT` | `/admin/limits/{key}` | Override limit for a key at runtime |
| `DELETE` | `/admin/limits/{key}` | Remove override, revert to plan default |
| `POST` | `/admin/config/reload` | Flush config cache on all instances |
| `GET` | `/admin/config/stats` | Keys, memory, algorithm distribution, circuit breaker states |

Admin API is blocked at the NGINX layer. Access it directly on `:8091` (Gateway 1) or `:8092` (Gateway 2).

**Cache invalidation flow:** `PUT /admin/limits/*` → Redis `SET` + `PUBLISH rl:cache-flush` → all gateway instances' subscriber goroutines flush their 30-second in-process cache → next request re-reads from Redis.

### Circuit Breaker + Retry

Per-backend circuit breaker (user, product, order services are isolated):

```
CLOSED — requests flow; failures are counted
  │ 5 consecutive failures
  ▼
OPEN — requests fail immediately (503), no backend contact
  │ 30 s timeout
  ▼
HALF-OPEN — one test request allowed through
  │ success                 │ failure
  ▼                         ▼
CLOSED                    OPEN
```

Retry policy for idempotent methods (GET, HEAD, PUT):
- Max 3 attempts
- Exponential backoff: 50 ms → 100 ms
- ±20 ms random jitter per delay — prevents thundering herd when N goroutines all back off and retry at the same instant
- POST and DELETE are **never** retried — the first attempt may have partially executed

### Distributed Rate Limiting (the thesis)

60 requests to Gateway 1 + 40 to Gateway 2 = `429 Too Many Requests` on request 101 to **either** instance. Rate-limit state lives in Redis, not in gateway memory, so it is shared across all instances atomically.

```bash
# Proven by the integration test suite:
go test -tags integration -v ./tests/integration/...
```

Tests:
- `TestSharedRateLimit` — 60+40=100 succeed, request 101 to either instance = 429
- `TestJWTPortability` — token from gateway1 accepted by gateway2; cross-instance auth
- `TestAdminPropagation` — override set on gateway1 propagates to gateway2 via pub/sub in <100 ms
- `TestNGINXRoundRobin` — 50/50 traffic split confirmed; admin API blocked at 403

---

## Quick Start

**Prerequisites:** Docker Desktop

```bash
git clone https://github.com/Teslaspacexhyperloop/Distributed-Rate-Limiter.git
cd Distributed-Rate-Limiter
docker compose up --build
```

Services after startup:

| URL | Description |
|---|---|
| `http://localhost:8090` | NGINX load balancer (public entry point) |
| `http://localhost:8091` | Gateway 1 direct (admin API access) |
| `http://localhost:8092` | Gateway 2 direct (admin API access) |
| `localhost:6380` | Redis (host port; internal port 6379) |

---

## Usage

### Register and make authenticated requests

```bash
# Register a user (pro plan, sliding window algorithm)
curl -X POST http://localhost:8090/auth/register \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"secret","plan":"pro","algorithm":"SLIDING_WINDOW"}'
# → {"token":"eyJ...","plan":"pro","sub":"<uuid>"}

# Login
curl -X POST http://localhost:8090/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"alice","password":"secret"}'

# Authenticated API call — rate limited by user ID (500 req/min, sliding window)
curl http://localhost:8090/api/products \
  -H "Authorization: Bearer eyJ..."
# X-RateLimit-Remaining: 499
# X-RateLimit-Algorithm: SLIDING_WINDOW

# Unauthenticated — rate limited by IP (100 req/min, token bucket)
curl http://localhost:8090/api/users
# X-RateLimit-Remaining: 99
# X-RateLimit-Algorithm: TOKEN_BUCKET
```

### Admin API

```bash
# Override a specific user's limit at runtime (no restart needed)
curl -X PUT http://localhost:8091/admin/limits/rate-limit:<uuid>:/api/products \
  -H "Content-Type: application/json" \
  -d '{"algorithm":"SLIDING_WINDOW","limit":1000,"windowSecs":60}'

# Check circuit breaker states
curl http://localhost:8091/admin/config/stats
# → {"circuit_breakers":{"order-service":"closed","product-service":"open",...}}

# Remove override (reverts to plan default)
curl -X DELETE http://localhost:8091/admin/limits/rate-limit:<uuid>:/api/products

# Flush config cache across all instances
curl -X POST http://localhost:8091/admin/config/reload
```

### See circuit breaker in action

```bash
# Stop a backend
docker stop distributed-rate-limiter-product-service-1

# First request — gateway retries 3× then returns 502
curl http://localhost:8091/api/products
# {"error":"backend unavailable"}

# Second request onward — circuit is OPEN, instant 503
curl http://localhost:8091/api/products
# {"error":"service temporarily unavailable — circuit breaker open"}

# Other services unaffected (isolated per-backend breaker)
curl http://localhost:8091/api/users
# 200 OK

# Restart backend — after 30 s the breaker goes HALF-OPEN → CLOSED on success
docker start distributed-rate-limiter-product-service-1
```

### Run integration tests

```bash
docker run --rm \
  -e GATEWAY1_URL=http://host.docker.internal:8091 \
  -e GATEWAY2_URL=http://host.docker.internal:8092 \
  -e NGINX_URL=http://host.docker.internal:8090 \
  -v "$(pwd):/app" -w /app golang:1.25-alpine \
  go test -tags integration -v ./tests/integration/...
```

---

## Configuration

All settings are environment variables. Defaults work out of the box.

| Variable | Default | Description |
|---|---|---|
| `GATEWAY_PORT` | `8080` | Gateway listen port |
| `GATEWAY_ID` | hostname | Instance label in logs and `X-Gateway-Id` header |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `RATE_LIMIT_FAILURE_MODE` | `open` | `open` = allow all when Redis is down; `closed` = deny all |
| `JWT_SECRET` | `change-me-in-production` | **Must be identical across all gateway instances** |
| `JWT_TOKEN_TTL` | `24h` | Token validity period |
| `RATE_LIMIT_IP_WHITELIST` | *(empty)* | Comma-separated CIDRs that bypass rate limiting |
| `RATE_LIMIT_IP_BLACKLIST` | *(empty)* | Comma-separated CIDRs that receive 403 |

### Plan limits

| Plan | Token Bucket | Sliding Window | Fixed Window | Leaky Bucket |
|---|---|---|---|---|
| `free` | 100/min burst | 100/min | 100/min | 100/min |
| `pro` | 500/min burst | 500/min | 500/min | 500/min |
| `enterprise` | 2000/min burst | 2000/min | 2000/min | 2000/min |

---

## Distributed Systems Guarantees

| Guarantee | Mechanism |
|---|---|
| Atomic rate limiting across N instances | Redis Lua scripts — single-threaded execution, no TOCTOU race |
| Clock consistency | `redis.call('TIME')` inside Lua — all instances use Redis server clock, not gateway clocks |
| Config changes visible to all instances | Redis pub/sub cache invalidation — no TTL wait |
| Stateless auth across instances | JWT signed with shared secret; user store in Redis |
| Cascading failure isolation | Per-service circuit breaker — one bad backend cannot exhaust gateway goroutines |
| Redis failure handling | `RATE_LIMIT_FAILURE_MODE=open` (availability) or `closed` (safety) — explicit CAP theorem trade-off |
| Request correlation | `X-Request-Id` propagated from client through gateway to all backends |

### Known limitations (intentional)

- **NGINX is a single point of failure.** In production: DNS-based load balancing or anycast IP. NGINX is here to demonstrate the gateway tier, not production HA.
- **Redis restart resets all rate-limit buckets.** Users get a free window after restart. Redis AOF persistence eliminates this at the cost of write amplification.
- **Admin API is unauthenticated.** Protected at the network layer (blocked by NGINX; direct gateway ports should be firewalled in production).

---

## Project Structure

```
cmd/
  gateway/          ← main entry point: wires all components
  user-service/     ← mock backend
  product-service/  ← mock backend
  order-service/    ← mock backend
internal/
  auth/             ← JWT sign/parse, register/login handlers, middleware
  admin/            ← runtime config API (6 endpoints)
  config/           ← env-var loading for all services
  middleware/        ← RequestID, logging, rate-limit middleware
  proxy/            ← httputil.ReverseProxy with resilient transport
  ratelimiter/      ← 4 algorithms, Redis client, Lua scripts, config resolver
  resilience/       ← circuit breaker (gobreaker), retry policy, transport
  routing/          ← Chi router, middleware stack wiring
  security/         ← IP whitelist/blacklist (CIDR)
nginx/
  nginx.conf        ← upstream pool, admin block
tests/
  integration/      ← multi-gateway distributed proof (go:build integration)
```
