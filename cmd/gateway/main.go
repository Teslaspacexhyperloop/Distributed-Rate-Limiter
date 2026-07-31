// Command gateway is the reverse-proxy entry point: NGINX/clients ->
// gateway -> User/Product/Order services.
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

	"distributed-rate-limiter/internal/config"
	"distributed-rate-limiter/internal/ratelimiter"
	"distributed-rate-limiter/internal/routing"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.LoadGateway()
	redisCfg := config.LoadRedis()

	// Build rate limiter backed by Redis. If Redis is unavailable at startup,
	// log a warning and continue without rate limiting — fail-open at boot so
	// a Redis outage doesn't prevent the gateway from starting.
	var rl *ratelimiter.RateLimiter
	rc, err := ratelimiter.NewRedisClient(
		context.Background(),
		redisCfg.Addr,
		redisCfg.Password,
		redisCfg.DB,
		redisCfg.PoolSize,
	)
	if err != nil {
		logger.Warn("Redis unavailable at startup — rate limiting disabled", "error", err)
	} else {
		resolver := ratelimiter.NewConfigResolver(rc.Client(), 5*time.Minute)
		failOpen := redisCfg.FailureMode != "closed"
		rl = ratelimiter.New(rc, resolver, failOpen)
		logger.Info("rate limiter ready",
			"redis_addr", redisCfg.Addr,
			"failure_mode", redisCfg.FailureMode,
		)
		defer rc.Close()
	}

	router, err := routing.NewRouter(cfg, logger, rl)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
