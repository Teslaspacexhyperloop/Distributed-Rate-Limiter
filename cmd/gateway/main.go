// Command gateway is the reverse-proxy entry point: NGINX/clients →
// gateway → User/Product/Order services.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"distributed-rate-limiter/internal/admin"
	authpkg "distributed-rate-limiter/internal/auth"
	"distributed-rate-limiter/internal/config"
	"distributed-rate-limiter/internal/ratelimiter"
	"distributed-rate-limiter/internal/routing"
	"distributed-rate-limiter/internal/security"
)

func main() {
	// GATEWAY_ID distinguishes instances in logs and response headers.
	// Falls back to hostname so local runs without Docker still have an ID.
	gatewayID := os.Getenv("GATEWAY_ID")
	if gatewayID == "" {
		if h, err := os.Hostname(); err == nil {
			gatewayID = h
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("gateway_id", gatewayID)

	cfg := config.LoadGateway()
	redisCfg := config.LoadRedis()
	authCfg := config.LoadAuth()
	secCfg := config.LoadSecurity()

	// IP filter — blacklist blocks before any processing; whitelist skips rate limiting.
	ipFilter, err := security.NewIPFilter(secCfg.IPWhitelist, secCfg.IPBlacklist)
	if err != nil {
		logger.Error("invalid IP filter config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Redis — all rate-limit state and user storage live here.
	rc, err := ratelimiter.NewRedisClient(
		ctx,
		redisCfg.Addr,
		redisCfg.Password,
		redisCfg.DB,
		redisCfg.PoolSize,
	)

	var opts routing.Options
	opts.GatewayID = gatewayID
	opts.IPFilter = ipFilter
	opts.AuthCfg = &authCfg

	if err != nil {
		logger.Warn("Redis unavailable at startup — rate limiting and auth storage disabled", "error", err)
	} else {
		defer rc.Close()

		resolver := ratelimiter.NewConfigResolver(rc.Client(), 30*time.Second)
		failOpen := redisCfg.FailureMode != "closed"

		opts.RateLimiter = ratelimiter.New(rc, resolver, failOpen)
		opts.AuthHandler = authpkg.NewHandler(rc.Client(), authCfg.JWTSecret, authCfg.TokenTTL)
		opts.AdminHandler = admin.NewHandler(resolver, rc.Client())

		// Subscribe to the cache-flush pub/sub channel. When any gateway instance
		// calls POST /admin/config/reload or PUT /admin/limits/*, it publishes here
		// and every instance (including itself) flushes its local config cache.
		go subscribeCacheFlush(ctx, rc, resolver, logger)

		logger.Info("rate limiter ready",
			"redis_addr", redisCfg.Addr,
			"failure_mode", redisCfg.FailureMode,
		)
	}

	router, err := routing.NewRouter(cfg, logger, opts)
	if err != nil {
		logger.Error("failed to build router", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	go func() {
		logger.Info("gateway listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("gateway server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down gateway")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGracePeriod)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

// subscribeCacheFlush listens on the Redis pub/sub channel "rl:cache-flush".
// Any gateway instance that modifies rate-limit config publishes to this channel
// so all instances flush their in-process config cache immediately, rather than
// waiting for the 30-second local-cache TTL to expire.
func subscribeCacheFlush(ctx context.Context, rc *ratelimiter.RedisClient, resolver *ratelimiter.ConfigResolver, logger *slog.Logger) {
	sub := rc.Client().Subscribe(ctx, "rl:cache-flush")
	defer sub.Close()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			resolver.FlushCache()
			logger.Info("config cache flushed via pub/sub", "trigger", msg.Payload)
		}
	}
}
