// Package config loads environment-derived settings for the gateway and
// mock backend services. Config is read once at startup and never mutated —
// the process keeps no in-memory state that a second instance wouldn't also
// have, since Phase 4 requires running multiple stateless gateways behind a
// load balancer.
package config

import (
	"os"
	"time"
)

// Gateway holds every setting the gateway needs at startup.
type Gateway struct {
	Port                string
	UserServiceURL      string
	ProductServiceURL   string
	OrderServiceURL     string
	ReadHeaderTimeout   time.Duration
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	ShutdownGracePeriod time.Duration
}

// LoadGateway reads gateway configuration from the environment, falling
// back to sane local-dev defaults for anything unset.
func LoadGateway() Gateway {
	return Gateway{
		Port:                getEnv("GATEWAY_PORT", "8080"),
		UserServiceURL:      getEnv("USER_SERVICE_URL", "http://localhost:8081"),
		ProductServiceURL:   getEnv("PRODUCT_SERVICE_URL", "http://localhost:8082"),
		OrderServiceURL:     getEnv("ORDER_SERVICE_URL", "http://localhost:8083"),
		ReadHeaderTimeout:   getEnvDuration("GATEWAY_READ_HEADER_TIMEOUT", 3*time.Second),
		ReadTimeout:         getEnvDuration("GATEWAY_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:        getEnvDuration("GATEWAY_WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:         getEnvDuration("GATEWAY_IDLE_TIMEOUT", 120*time.Second),
		ShutdownGracePeriod: getEnvDuration("GATEWAY_SHUTDOWN_GRACE_PERIOD", 10*time.Second),
	}
}

// Backend holds the settings a mock backend service needs. Each service is
// an independent binary so it can be scaled or replaced without touching
// the gateway.
type Backend struct {
	Name string
	Port string
}

// LoadBackend reads a backend service's port from PORT, falling back to
// defaultPort for local runs outside Docker Compose.
func LoadBackend(name, defaultPort string) Backend {
	return Backend{Name: name, Port: getEnv("PORT", defaultPort)}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
