package ratelimiter

import (
	"context"
	"fmt"
)

// LeakyBucket drains at a constant Rate requests per second regardless of
// burst size, protecting downstream services from traffic spikes.
//
// Unlike TokenBucket (which rejects excess requests immediately), LeakyBucket
// queues them up to Capacity. A response with Allowed=true and RetryAfter>0
// means "accepted into the queue — delay the actual downstream call by
// RetryAfter before processing." Allowed=false means the queue is full; reject.
//
// This models the choice between reject-on-overflow (Token Bucket) vs
// queue-on-overflow (Leaky Bucket) — a design decision with different failure
// modes: Token Bucket is faster to shed load, Leaky Bucket smooths traffic.
type LeakyBucket struct {
	rc       *RedisClient
	rate     float64 // drain rate: requests per second
	capacity int64   // max queue depth
}

func (lb *LeakyBucket) Algorithm() Algorithm { return AlgorithmLeakyBucket }

func (lb *LeakyBucket) Allow(ctx context.Context, key string, cost int64) (Result, error) {
	raw, err := lb.rc.EvalScript(ctx, AlgorithmLeakyBucket,
		[]string{key},
		lb.rate,
		lb.capacity,
		cost,
	)
	if err != nil {
		return Result{}, fmt.Errorf("leaky bucket %q: %w", key, err)
	}
	return parseResult(raw, AlgorithmLeakyBucket)
}
