package ratelimiter

import "context"

// RateLimiter is the main entry point for rate limiting. It ties together the
// ConfigResolver (which algorithm and limit apply), the four algorithm
// implementations (which Lua script to run), and the fail-mode policy (what
// to do when Redis is unavailable).
type RateLimiter struct {
	rc       *RedisClient
	resolver *ConfigResolver
	failOpen bool // true = allow all requests when Redis is down
}

// New creates a RateLimiter. Set failOpen=true for availability (requests pass
// through when Redis is unreachable) or failOpen=false for safety (requests are
// rejected). This is the CAP theorem trade-off: availability vs consistency.
func New(rc *RedisClient, resolver *ConfigResolver, failOpen bool) *RateLimiter {
	return &RateLimiter{rc: rc, resolver: resolver, failOpen: failOpen}
}

// Allow checks whether the request identified by (userID, route, plan) is
// within its rate limit. It:
//  1. Resolves the applicable LimitConfig and Redis key
//  2. Instantiates the right algorithm
//  3. Runs the Lua script atomically in Redis
//  4. Applies the fail-mode policy if Redis is unreachable
func (rl *RateLimiter) Allow(ctx context.Context, userID, route, plan string) (Result, error) {
	cfg, key := rl.resolver.Resolve(ctx, userID, route, plan)

	result, err := newLimiter(rl.rc, cfg).Allow(ctx, key, 1)
	if err != nil {
		if rl.failOpen {
			// Fail-open: let the request through, don't surface the error.
			return Result{Allowed: true, Algorithm: cfg.Algorithm}, nil
		}
		// Fail-closed: reject with the Redis error surfaced to the caller.
		return Result{Allowed: false, Algorithm: cfg.Algorithm}, err
	}
	return result, nil
}
