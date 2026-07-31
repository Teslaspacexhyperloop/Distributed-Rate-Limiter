// Package ratelimiter implements four rate-limiting algorithms (Token Bucket,
// Sliding Window, Fixed Window, Leaky Bucket), all backed by Redis Lua scripts
// that execute atomically — preventing the TOCTOU race where two gateway
// instances both read "1 token left", both allow, and both decrement.
package ratelimiter

import (
	"context"
	"fmt"
	"time"
)

// Algorithm identifies which rate-limiting algorithm processed a request.
type Algorithm string

const (
	AlgorithmTokenBucket   Algorithm = "TOKEN_BUCKET"
	AlgorithmSlidingWindow Algorithm = "SLIDING_WINDOW"
	AlgorithmFixedWindow   Algorithm = "FIXED_WINDOW"
	AlgorithmLeakyBucket   Algorithm = "LEAKY_BUCKET"
)

// Result is the outcome of a single rate-limit check.
type Result struct {
	Allowed    bool
	Remaining  int64         // tokens / slots left after this request
	RetryAfter time.Duration // how long to wait before retrying; 0 when Allowed
	Algorithm  Algorithm
}

// Limiter is the common interface all four algorithm implementations satisfy.
// Each implementation is stateless — all mutable bucket/window state lives in
// Redis, so every gateway instance shares the same view of the limit.
type Limiter interface {
	Allow(ctx context.Context, key string, cost int64) (Result, error)
	Algorithm() Algorithm
}

// newLimiter instantiates the concrete Limiter described by cfg.
func newLimiter(rc *RedisClient, cfg LimitConfig) Limiter {
	switch cfg.Algorithm {
	case AlgorithmSlidingWindow:
		return &SlidingWindow{rc: rc, limit: cfg.Limit, windowSecs: cfg.WindowSecs}
	case AlgorithmFixedWindow:
		return &FixedWindow{rc: rc, limit: cfg.Limit, windowSecs: cfg.WindowSecs}
	case AlgorithmLeakyBucket:
		return &LeakyBucket{rc: rc, rate: cfg.Rate, capacity: cfg.Capacity}
	default:
		return &TokenBucket{rc: rc, capacity: cfg.Capacity, refillRate: cfg.RefillRate}
	}
}

// parseResult converts the []interface{}{allowed, remaining, wait_ms} array
// that every Lua script returns into a typed Result.
func parseResult(raw interface{}, algo Algorithm) (Result, error) {
	vals, ok := raw.([]interface{})
	if !ok || len(vals) < 3 {
		return Result{}, fmt.Errorf("unexpected script result %T: %v", raw, raw)
	}
	waitMs := toInt64(vals[2])
	return Result{
		Allowed:    toInt64(vals[0]) == 1,
		Remaining:  toInt64(vals[1]),
		RetryAfter: time.Duration(waitMs) * time.Millisecond,
		Algorithm:  algo,
	}, nil
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
