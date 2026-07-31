package auth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey int

const claimsKey contextKey = 0

// Middleware validates the Bearer JWT in the Authorization header. If the
// header is absent the request continues unauthenticated — ClaimsFromContext
// returns nil and rate limiting falls back to IP + free-plan behaviour.
// If a token is present but invalid the request is rejected with 401.
//
// IMPORTANT: the Authorization header is intentionally NOT forwarded to the
// logging middleware or backend services so tokens never appear in logs.
func Middleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearer(r)
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}

			claims, err := Parse(token, secret)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid or expired token"}`))
				return
			}

			ctx := context.WithValue(r.Context(), claimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClaimsFromContext returns the JWT claims stored by Middleware, or nil when
// the request had no Authorization header or an invalid token.
func ClaimsFromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey).(*Claims)
	return c
}

func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}
