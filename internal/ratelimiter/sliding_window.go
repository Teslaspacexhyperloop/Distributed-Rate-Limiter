package ratelimiter

import (
	"context"
	"fmt"
)

// SlidingWindow enforces an exact rolling window: at most Limit requests are
// allowed in any WindowSecs-second interval ending at the current moment.
//
// Unlike FixedWindow it is immune to the boundary-burst attack — there is no
// reset boundary to exploit. The cost is higher Redis memory usage: one
// sorted-set member per request, versus FixedWindow's single integer counter.
type SlidingWindow struct {
	rc         *RedisClient
	limit      int64
	windowSecs int64
}

func (sw *SlidingWindow) Algorithm() Algorithm { return AlgorithmSlidingWindow }

func (sw *SlidingWindow) Allow(ctx context.Context, key string, cost int64) (Result, error) {
	raw, err := sw.rc.EvalScript(ctx, AlgorithmSlidingWindow,
		[]string{key},
		sw.limit,
		sw.windowSecs,
		cost,
	)
	if err != nil {
		return Result{}, fmt.Errorf("sliding window %q: %w", key, err)
	}
	return parseResult(raw, AlgorithmSlidingWindow)
}
