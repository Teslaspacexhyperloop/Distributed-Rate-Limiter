// Package routing wires the gateway's HTTP router.
package routing

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"distributed-rate-limiter/internal/config"
	custommw "distributed-rate-limiter/internal/middleware"
	"distributed-rate-limiter/internal/proxy"
)

// NewRouter wires the gateway's middleware stack and proxies each /api
// prefix to its backend service. Routing is static and config-driven — no
// in-memory state that would break horizontal scaling in Phase 4.
func NewRouter(cfg config.Gateway, logger *slog.Logger) (http.Handler, error) {
	r := chi.NewRouter()

	r.Use(custommw.RequestID)
	r.Use(custommw.Logging(logger))
	r.Use(chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	backends := []struct {
		prefix string
		url    string
		name   string
	}{
		{"/api/users", cfg.UserServiceURL, "user-service"},
		{"/api/products", cfg.ProductServiceURL, "product-service"},
		{"/api/orders", cfg.OrderServiceURL, "order-service"},
	}

	for _, b := range backends {
		target, err := url.Parse(b.url)
		if err != nil {
			return nil, fmt.Errorf("parsing %s url %q: %w", b.name, b.url, err)
		}

		backendProxy := proxy.New(b.name, target)
		r.Handle(b.prefix, backendProxy)
		r.Handle(b.prefix+"/*", backendProxy)
	}

	return r, nil
}
