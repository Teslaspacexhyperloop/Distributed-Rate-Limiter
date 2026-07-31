package ratelimiter

import (
	"context"
	"fmt"
)

// TokenBucket allows bursts up to Capacity tokens and refills at RefillRate
// tokens per second. The bucket absorbs short bursts gracefully — a user can
// fire 100 requests instantly if they have 100 tokens banked, unlike
// SlidingWindow which enforces an exact per-second cap.
//
// Classic boundary-burst attack: this algorithm is immune. Tokens don't reset
// at interval boundaries; they refill continuously based on elapsed time.
type TokenBucket struct {
	rc         *RedisClient
	capacity   int64
	refillRate float64 // tokens added per second
}

func (tb *TokenBucket) Algorithm() Algorithm { return AlgorithmTokenBucket }

// Allow consumes cost tokens from the bucket for key.
// If fewer than cost tokens are available, the request is rejected and
// RetryAfter indicates how long until enough tokens refill.
func (tb *TokenBucket) Allow(ctx context.Context, key string, cost int64) (Result, error) {
	raw, err := tb.rc.EvalScript(ctx, AlgorithmTokenBucket,
		[]string{key},
		tb.capacity,
		tb.refillRate,
		cost,
	)
	if err != nil {
		return Result{}, fmt.Errorf("token bucket %q: %w", key, err)
	}
	return parseResult(raw, AlgorithmTokenBucket)
}
