package middleware

import (
	"net/http"
	"strconv"

	"distributed-rate-limiter/internal/ratelimiter"
)

// RateLimit enforces request limits using rl. The subject key is the client
// IP for Phase 2; Phase 3 replaces this with the JWT sub claim once the
// authentication middleware runs first (it will write the user ID into the
// request context).
func RateLimit(rl *ratelimiter.RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := clientIP(r)

			// Phase 2: all requests use the "free" plan.
			// Phase 3: read plan from the JWT context value set by auth middleware.
			result, err := rl.Allow(r.Context(), userID, r.URL.Path, "free")
			if err != nil {
				// Non-nil error here means fail-closed rejected the request.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"rate limiter unavailable"}`))
				return
			}

			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(result.Remaining, 10))
			w.Header().Set("X-RateLimit-Algorithm", string(result.Algorithm))

			if !result.Allowed {
				retryAfterSecs := int64(result.RetryAfter.Seconds())
				if retryAfterSecs < 1 {
					retryAfterSecs = 1
				}
				w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSecs, 10))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientIP extracts the real client IP. Prefers X-Forwarded-For set by NGINX
// (active in Phase 4); falls back to RemoteAddr for local development.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}
