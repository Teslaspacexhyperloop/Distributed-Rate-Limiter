package ratelimiter

import (
	"context"
	"fmt"
)

// FixedWindow resets the request counter at each interval boundary. It is the
// most memory-efficient algorithm (one integer key per window) but is
// vulnerable to the boundary-burst attack: a client can send Limit requests
// at 11:59:59 and Limit more at 12:00:01, getting 2×Limit requests in 2 seconds.
// TokenBucket and SlidingWindow both prevent this; see tests/concurrency/.
type FixedWindow struct {
	rc         *RedisClient
	limit      int64
	windowSecs int64
}

func (fw *FixedWindow) Algorithm() Algorithm { return AlgorithmFixedWindow }

func (fw *FixedWindow) Allow(ctx context.Context, key string, cost int64) (Result, error) {
	raw, err := fw.rc.EvalScript(ctx, AlgorithmFixedWindow,
		[]string{key},
		fw.limit,
		fw.windowSecs,
		cost,
	)
	if err != nil {
		return Result{}, fmt.Errorf("fixed window %q: %w", key, err)
	}
	return parseResult(raw, AlgorithmFixedWindow)
}
