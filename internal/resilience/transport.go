package resilience

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sony/gobreaker"
)

// ErrCircuitOpen is returned when a backend's circuit breaker is OPEN and the
// request is rejected immediately without contacting the backend.
type ErrCircuitOpen struct{ Service string }

func (e *ErrCircuitOpen) Error() string {
	return fmt.Sprintf("circuit breaker open for %s — backend assumed unhealthy", e.Service)
}

// Transport wraps an http.RoundTripper with per-service circuit breaking and
// idempotent-method retry with exponential backoff + jitter.
//
// On each attempt the circuit breaker is consulted:
//   - CLOSED   → allow; record success/failure after the attempt
//   - OPEN     → reject immediately with ErrCircuitOpen (no network call)
//   - HALF-OPEN → allow one test request; close on success, re-open on failure
//
// Retries happen only for idempotent methods (GET, HEAD, PUT) on 502/503/504.
// POST and DELETE are never retried because their first attempt may have
// partially executed on the backend.
type Transport struct {
	name    string
	inner   http.RoundTripper
	breaker *gobreaker.TwoStepCircuitBreaker
	policy  Policy
}

// NewTransport creates a resilient transport. name identifies the backend in
// errors and logs. breaker may be nil to disable circuit breaking.
func NewTransport(name string, inner http.RoundTripper, breaker *gobreaker.TwoStepCircuitBreaker, policy Policy) *Transport {
	return &Transport{name: name, inner: inner, breaker: breaker, policy: policy}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxAttempts := 1
	if IsIdempotent(req.Method) {
		maxAttempts = t.policy.MaxAttempts
	}

	// Buffer the request body once so it can be replayed on retry.
	// For GET/HEAD this is always nil and no allocation occurs.
	var bodyBuf []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBuf, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("resilient transport: reading request body: %w", err)
		}
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := t.policy.Backoff(attempt)
			slog.Info("retrying backend request",
				"service", t.name,
				"attempt", attempt+1,
				"max_attempts", maxAttempts,
				"delay_ms", delay.Milliseconds(),
			)
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(delay):
			}
		}

		// Restore the body for each attempt.
		if bodyBuf != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBuf))
			req.ContentLength = int64(len(bodyBuf))
		}

		resp, err := t.doAttempt(req)
		if err != nil {
			lastErr = err
			// Don't retry if the circuit opened or context was cancelled —
			// retrying would not help in either case.
			if _, open := err.(*ErrCircuitOpen); open {
				return nil, err
			}
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			continue
		}

		// On the last attempt, return whatever the backend sent (even 5xx).
		// On intermediate attempts, retry retriable status codes.
		if IsRetriable(resp.StatusCode) && attempt < maxAttempts-1 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s returned %d", t.name, resp.StatusCode)
			continue
		}

		return resp, nil
	}

	return nil, lastErr
}

// doAttempt makes a single request, consulting the circuit breaker before and
// after. It returns ErrCircuitOpen without making a network call when the
// breaker is OPEN.
func (t *Transport) doAttempt(req *http.Request) (*http.Response, error) {
	if t.breaker == nil {
		return t.inner.RoundTrip(req)
	}

	done, err := t.breaker.Allow()
	if err != nil {
		// gobreaker returns ErrOpenState when the breaker is in the OPEN state.
		return nil, &ErrCircuitOpen{Service: t.name}
	}

	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		done(false) // network-level failure
		return nil, err
	}

	// 5xx responses are breaker failures. 4xx are client errors (not backend
	// health) and must not count against the breaker — this prevents a burst of
	// bad requests from tripping the breaker and taking down a healthy service.
	done(resp.StatusCode < 500)
	return resp, nil
}
