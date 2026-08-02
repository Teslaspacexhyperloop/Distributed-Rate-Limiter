// Package proxy builds reverse proxies to the gateway's backend services.
package proxy

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"distributed-rate-limiter/internal/middleware"
	"distributed-rate-limiter/internal/resilience"
)

// backendTransport is shared by every backend proxy. Timeouts live here, at
// the transport layer, rather than per-handler — a single hung backend must
// never be able to hold a gateway goroutine open indefinitely.
var backendTransport = &http.Transport{
	DialContext: (&net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSHandshakeTimeout:   3 * time.Second,
	ResponseHeaderTimeout: 5 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
}

// New builds a reverse proxy for a single backend. If breaker is non-nil each
// request is wrapped in a circuit breaker and idempotent methods are retried
// with exponential backoff + jitter on 502/503/504 responses.
//
// breaker is per-backend so a failing product-service does not trip the breaker
// for user-service or order-service.
func New(name string, target *url.URL, breaker *resilience.Breaker, policy resilience.Policy) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)

	if breaker != nil {
		rp.Transport = resilience.NewTransport(name, backendTransport, breaker, policy)
	} else {
		rp.Transport = backendTransport
	}

	baseDirector := rp.Director
	rp.Director = func(r *http.Request) {
		baseDirector(r)
		r.Header.Set(middleware.RequestIDHeader, middleware.FromContext(r.Context()))
	}

	// The gateway's RequestID middleware already set this header on the
	// client-facing ResponseWriter. ReverseProxy copies backend response
	// headers with Add (not Set), so without this the header would double up
	// into a malformed "id,id" value.
	rp.ModifyResponse = func(res *http.Response) error {
		res.Header.Del(middleware.RequestIDHeader)
		return nil
	}

	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		var cbErr *resilience.ErrCircuitOpen
		if errors.As(err, &cbErr) {
			// Circuit is OPEN — the backend is known unhealthy; fail fast.
			slog.Warn("circuit breaker open, rejecting request",
				"service", name,
				"request_id", middleware.FromContext(r.Context()),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"service temporarily unavailable — circuit breaker open"}`))
			return
		}

		slog.Error("backend unavailable",
			"service", name,
			"request_id", middleware.FromContext(r.Context()),
			"error", err,
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"backend unavailable"}`))
	}

	return rp
}
