package ratelimiter

import "context"

// RateLimiter is the main entry point for rate limiting. It ties together the
// ConfigResolver, the four algorithm implementations, and the fail-mode policy.
type RateLimiter struct {
	rc       *RedisClient
	resolver *ConfigResolver
	failOpen bool
}

// New creates a RateLimiter. failOpen=true passes requests through when Redis
// is unreachable (availability). failOpen=false rejects them (safety).
func New(rc *RedisClient, resolver *ConfigResolver, failOpen bool) *RateLimiter {
	return &RateLimiter{rc: rc, resolver: resolver, failOpen: failOpen}
}

// Allow checks whether the request identified by (userID, route, plan) is
// within its rate limit. algoHint is the preferred algorithm from the JWT
// claim ("" = use plan default). The resolver picks the highest-priority
// applicable config and the corresponding Lua script runs atomically in Redis.
func (rl *RateLimiter) Allow(ctx context.Context, userID, route, plan, algoHint string) (Result, error) {
	cfg, key := rl.resolver.Resolve(ctx, userID, route, plan, algoHint)

	result, err := newLimiter(rl.rc, cfg).Allow(ctx, key, 1)
	if err != nil {
		if rl.failOpen {
			return Result{Allowed: true, Algorithm: cfg.Algorithm}, nil
		}
		return Result{Allowed: false, Algorithm: cfg.Algorithm}, err
	}
	return result, nil
}
