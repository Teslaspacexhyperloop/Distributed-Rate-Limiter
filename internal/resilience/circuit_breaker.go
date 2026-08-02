// Package resilience provides circuit breaking and retry for backend calls.
// Each backend service gets its own circuit breaker so a single failing
// service cannot cause cascading failures across unrelated routes.
package resilience

import (
	"log/slog"
	"time"

	"github.com/sony/gobreaker"
)

// BreakerConfig controls circuit breaker thresholds.
type BreakerConfig struct {
	// ConsecutiveFailures is the number of consecutive backend failures before
	// the breaker opens. Lower values respond faster to failures; higher values
	// tolerate transient errors better.
	ConsecutiveFailures uint32
	// Timeout is how long the breaker stays OPEN before allowing one test
	// request through (HALF-OPEN). If the test succeeds the breaker closes.
	Timeout time.Duration
	// HalfOpenRequests is the number of test requests permitted while HALF-OPEN.
	HalfOpenRequests uint32
}

// DefaultBreakerConfig returns conservative defaults that trip quickly and
// recover cautiously — appropriate for a learning/demo environment.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		ConsecutiveFailures: 5,
		Timeout:             30 * time.Second,
		HalfOpenRequests:    1,
	}
}

// Breaker is an alias for gobreaker.TwoStepCircuitBreaker exposed so callers
// don't need to import the gobreaker package directly.
type Breaker = gobreaker.TwoStepCircuitBreaker

// NewBreaker creates a named two-step circuit breaker. name must be unique per
// backend service (e.g. "user-service") so each service trips independently.
//
// State machine:
//
//	CLOSED   — requests flow normally; failures are counted
//	OPEN     — requests fail immediately; no backend contact
//	HALF-OPEN — one test request is sent; success → CLOSED, failure → OPEN
func NewBreaker(name string, cfg BreakerConfig, logger *slog.Logger) *gobreaker.TwoStepCircuitBreaker {
	return gobreaker.NewTwoStepCircuitBreaker(gobreaker.Settings{
		Name:        name,
		MaxRequests: cfg.HalfOpenRequests,
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= cfg.ConsecutiveFailures
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			logger.Warn("circuit breaker state changed",
				"service", name,
				"from", from.String(),
				"to", to.String(),
			)
		},
	})
}
