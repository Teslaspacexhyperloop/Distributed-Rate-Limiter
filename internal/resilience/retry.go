package resilience

import (
	"math/rand"
	"net/http"
	"time"
)

// Policy configures retry behaviour. Retries only apply to idempotent HTTP
// methods (GET, HEAD, PUT) — non-idempotent operations (POST, DELETE, PATCH)
// may have partially executed on the backend and must not be replayed.
type Policy struct {
	MaxAttempts  int           // total attempts including the first (1 = no retry)
	InitialDelay time.Duration // pause before the second attempt
	Multiplier   float64       // each subsequent delay is multiplied by this
	MaxJitter    time.Duration // ± random jitter added to each delay
}

// DefaultPolicy returns a conservative retry policy with exponential backoff.
// Three total attempts with delays of ~50 ms and ~100 ms prevent retry
// amplification while giving a briefly overloaded backend time to recover.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:  3,
		InitialDelay: 50 * time.Millisecond,
		Multiplier:   2.0,
		MaxJitter:    20 * time.Millisecond,
	}
}

// Backoff returns the delay before attempt n (0-indexed).
// n=0 → 0 (no wait before the first try)
// n=1 → InitialDelay ± jitter
// n=2 → InitialDelay×Multiplier ± jitter
func (p Policy) Backoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := p.InitialDelay
	for i := 1; i < n; i++ {
		d = time.Duration(float64(d) * p.Multiplier)
	}
	// Symmetric jitter: random value in [-MaxJitter, +MaxJitter].
	// Spreading retry timing prevents the thundering-herd problem where N
	// goroutines all back off for exactly the same duration and then all
	// hammer the recovering backend at the same instant.
	jitterRange := int64(p.MaxJitter) * 2
	jitter := time.Duration(rand.Int63n(jitterRange+1)) - p.MaxJitter
	if d+jitter < 0 {
		return 0
	}
	return d + jitter
}

// IsIdempotent reports whether the HTTP method is safe to retry. A second
// attempt at a GET or PUT returns the same result; a second POST may create
// a duplicate resource.
func IsIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodOptions:
		return true
	default:
		return false
	}
}

// IsRetriable reports whether an HTTP status code warrants a retry.
// 502 Bad Gateway, 503 Service Unavailable, and 504 Gateway Timeout all
// indicate a transient backend problem that may resolve on the next attempt.
func IsRetriable(code int) bool {
	return code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}
