package middleware

import (
	"net/http"
	"strconv"

	"distributed-rate-limiter/internal/auth"
	"distributed-rate-limiter/internal/ratelimiter"
	"distributed-rate-limiter/internal/security"
)

// RateLimit enforces request limits. It integrates with both auth and IP-filter
// middleware that run earlier in the stack:
//
//   - If IPFilter marked this IP as whitelisted → skip rate limiting entirely.
//   - If JWT middleware set claims → use claims.UserID() and claims.Plan as the
//     rate-limit subject and tier. claims.Algorithm is passed as a hint so users
//     can request a specific algorithm (e.g. SLIDING_WINDOW over TOKEN_BUCKET).
//   - Otherwise → fall back to client IP as subject and "free" plan.
func RateLimit(rl *ratelimiter.RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Whitelisted IPs bypass rate limiting entirely (e.g. internal services).
			if security.IsWhitelisted(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}

			userID := clientIP(r)
			plan := "free"
			algoHint := ""

			if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
				userID = claims.UserID()
				plan = claims.Plan
				algoHint = claims.Algorithm
			}

			result, err := rl.Allow(r.Context(), userID, r.URL.Path, plan, algoHint)
			if err != nil {
				// Non-nil error only reaches here under fail-closed mode.
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

// clientIP extracts the real client IP. Prefers X-Forwarded-For (set by NGINX
// in Phase 4); falls back to RemoteAddr for direct connections.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}
