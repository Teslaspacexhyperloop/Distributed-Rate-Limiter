// Package middleware provides Chi-compatible HTTP middleware shared by the
// gateway and every mock backend service.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type contextKey int

const requestIDKey contextKey = 0

// RequestIDHeader is the header used to correlate a single logical request
// across every hop it takes: client -> gateway -> backend service.
const RequestIDHeader = "X-Request-Id"

// RequestID assigns a correlation ID to every request. It preserves an
// inbound ID if the caller already set one (e.g. an upstream gateway
// instance), otherwise it generates one, so a single request can be traced
// end to end in logs.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get(RequestIDHeader)
		if reqID == "" {
			reqID = newRequestID()
		}

		w.Header().Set(RequestIDHeader, reqID)
		ctx := context.WithValue(r.Context(), requestIDKey, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// FromContext extracts the correlation ID set by RequestID, returning "" if
// none is present (e.g. the middleware wasn't installed).
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}
