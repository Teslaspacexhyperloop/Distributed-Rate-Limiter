// Package routing wires the gateway's HTTP router.
package routing

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"distributed-rate-limiter/internal/admin"
	"distributed-rate-limiter/internal/auth"
	"distributed-rate-limiter/internal/config"
	custommw "distributed-rate-limiter/internal/middleware"
	"distributed-rate-limiter/internal/proxy"
	"distributed-rate-limiter/internal/ratelimiter"
	"distributed-rate-limiter/internal/resilience"
	"distributed-rate-limiter/internal/security"
)

// Options carries optional components. Fields may be nil — the router degrades
// gracefully (e.g. no rate limiting without RateLimiter).
type Options struct {
	GatewayID    string // emitted as X-Gateway-Id response header for instance identification
	RateLimiter  *ratelimiter.RateLimiter
	AuthCfg      *config.Auth
	IPFilter     *security.IPFilter
	AuthHandler  *auth.Handler
	AdminHandler *admin.Handler
	// Breakers holds one circuit breaker per backend service (keyed by name).
	// When set, each proxy is wrapped with circuit breaking and retry.
	Breakers map[string]*resilience.Breaker
}

// NewRouter wires the gateway's middleware stack and proxies each /api prefix
// to its backend service. The middleware order is:
//  1. GatewayID  — set X-Gateway-Id response header (Phase 4)
//  2. RequestID  — correlation ID for every hop
//  3. Logging    — structured JSON per request
//  4. Recoverer  — panic → 500
//  5. IPFilter   — blacklist → 403; whitelist → mark in context
//  6. JWT auth   — optional; sets claims in context
//  7. RateLimit  — uses claims + whitelist flag from context
func NewRouter(cfg config.Gateway, logger *slog.Logger, opts Options) (http.Handler, error) {
	r := chi.NewRouter()

	// Stamp every response with the instance ID so load-balancer tests and
	// Grafana dashboards can track which gateway handled each request.
	if opts.GatewayID != "" {
		gid := opts.GatewayID
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Gateway-Id", gid)
				next.ServeHTTP(w, r)
			})
		})
	}

	r.Use(custommw.RequestID)
	r.Use(custommw.Logging(logger))
	r.Use(chimw.Recoverer)

	if opts.IPFilter != nil {
		r.Use(opts.IPFilter.Middleware())
	}

	if opts.AuthCfg != nil {
		r.Use(auth.Middleware(opts.AuthCfg.JWTSecret))
	}

	if opts.RateLimiter != nil {
		r.Use(custommw.RateLimit(opts.RateLimiter))
	}

	// Health check — no auth, no rate limiting (it runs before the middleware stack
	// only conceptually; in Chi, Use() applies globally, but /healthz is typically
	// excluded from rate limiting by whitelisting the probe IP in RATE_LIMIT_IP_WHITELIST).
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Auth endpoints — unauthenticated by design.
	if opts.AuthHandler != nil {
		r.Post("/auth/register", opts.AuthHandler.Register)
		r.Post("/auth/login", opts.AuthHandler.Login)
	}

	// Admin API — unauthenticated in Phase 3; restrict at network layer via NGINX in Phase 4.
	if opts.AdminHandler != nil {
		admin.Mount(r, opts.AdminHandler)
	}

	// Backend proxies.
	backends := []struct {
		prefix string
		target string
		name   string
	}{
		{"/api/users", cfg.UserServiceURL, "user-service"},
		{"/api/products", cfg.ProductServiceURL, "product-service"},
		{"/api/orders", cfg.OrderServiceURL, "order-service"},
	}

	retryPolicy := resilience.DefaultPolicy()

	for _, b := range backends {
		target, err := url.Parse(b.target)
		if err != nil {
			return nil, fmt.Errorf("parsing %s url %q: %w", b.name, b.target, err)
		}
		var breaker *resilience.Breaker
		if opts.Breakers != nil {
			breaker = opts.Breakers[b.name]
		}
		p := proxy.New(b.name, target, breaker, retryPolicy)
		r.Handle(b.prefix, p)
		r.Handle(b.prefix+"/*", p)
	}

	return r, nil
}
