// Package proxy builds reverse proxies to the gateway's backend services.
package proxy

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"distributed-rate-limiter/internal/middleware"
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

// New builds a reverse proxy for a single backend. It propagates the
// request's correlation ID to the backend; X-Forwarded-For is populated
// automatically by httputil.ReverseProxy from the client's RemoteAddr.
func New(name string, target *url.URL) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = backendTransport

	baseDirector := rp.Director
	rp.Director = func(r *http.Request) {
		baseDirector(r)
		r.Header.Set(middleware.RequestIDHeader, middleware.FromContext(r.Context()))
	}

	// The gateway's RequestID middleware already set this header on the
	// client-facing ResponseWriter before the proxy ran. ReverseProxy copies
	// backend response headers with Header.Add rather than Set, so without
	// this the backend's copy of the same header would double up into a
	// single malformed "id,id" header instead of one value.
	rp.ModifyResponse = func(res *http.Response) error {
		res.Header.Del(middleware.RequestIDHeader)
		return nil
	}

	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
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
