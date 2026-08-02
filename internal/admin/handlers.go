// Package admin provides the runtime management API for the rate limiter.
// All writes go directly to Redis so every gateway instance sees changes
// immediately on their next request (within cache TTL) or on pub/sub flush.
package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"

	"distributed-rate-limiter/internal/ratelimiter"
	"distributed-rate-limiter/internal/resilience"
)

// pubsubChannel is the Redis pub/sub channel used to broadcast cache-flush
// events to all gateway instances. When any instance calls Reload, it publishes
// here; all instances (including itself) flush their local config cache.
const pubsubChannel = "rl:cache-flush"

// Handler exposes the admin API endpoints. It needs access to the
// ConfigResolver for override CRUD and to the Redis client for key scanning,
// stats, and pub/sub publishing.
type Handler struct {
	resolver *ratelimiter.ConfigResolver
	rdb      *redis.Client
	breakers map[string]*resilience.Breaker
}

// NewHandler creates an admin Handler. breakers may be nil.
func NewHandler(resolver *ratelimiter.ConfigResolver, rdb *redis.Client, breakers map[string]*resilience.Breaker) *Handler {
	return &Handler{resolver: resolver, rdb: rdb, breakers: breakers}
}

// ListKeys scans Redis for all active rate-limit counters.
// GET /admin/keys
func (h *Handler) ListKeys(w http.ResponseWriter, r *http.Request) {
	var keys []string
	iter := h.rdb.Scan(r.Context(), 0, "rate-limit:*", 0).Iterator()
	for iter.Next(r.Context()) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys, "count": len(keys)})
}

// GetLimit returns the current config override for a key.
// GET /admin/limits/*
func (h *Handler) GetLimit(w http.ResponseWriter, r *http.Request) {
	key := limitKey(r)
	cfg, ok := h.resolver.GetOverride(r.Context(), key)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no override set for this key"})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// SetLimit writes a config override for a key and broadcasts a cache-flush
// event so all gateway instances pick it up without restarting.
// PUT /admin/limits/*
func (h *Handler) SetLimit(w http.ResponseWriter, r *http.Request) {
	key := limitKey(r)

	var cfg ratelimiter.LimitConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if err := h.resolver.SetOverride(r.Context(), key, cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Notify all other instances so they evict their cached config for this key.
	h.rdb.Publish(r.Context(), pubsubChannel, key)

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "key": key})
}

// DeleteLimit removes a config override, returning the key to plan defaults.
// DELETE /admin/limits/*
func (h *Handler) DeleteLimit(w http.ResponseWriter, r *http.Request) {
	key := limitKey(r)

	if err := h.resolver.DeleteOverride(r.Context(), key); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	h.rdb.Publish(r.Context(), pubsubChannel, key)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "key": key})
}

// Reload flushes the local config cache on this instance and broadcasts a
// flush event to all other instances via Redis pub/sub.
// POST /admin/config/reload
func (h *Handler) Reload(w http.ResponseWriter, r *http.Request) {
	h.resolver.FlushCache()
	h.rdb.Publish(r.Context(), pubsubChannel, "global")
	writeJSON(w, http.StatusOK, map[string]string{"status": "cache flushed and event broadcast"})
}

// Stats returns a snapshot of rate-limit activity and Redis memory usage.
// GET /admin/config/stats
func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	// Count active rate-limit counter keys.
	var rlKeys int
	iter := h.rdb.Scan(r.Context(), 0, "rate-limit:*", 0).Iterator()
	for iter.Next(r.Context()) {
		rlKeys++
	}

	// Count per-key overrides and tally algorithm distribution.
	algoDist := make(map[string]int)
	iter2 := h.rdb.Scan(r.Context(), 0, "rl-config:*", 0).Iterator()
	for iter2.Next(r.Context()) {
		data, err := h.rdb.Get(r.Context(), iter2.Val()).Bytes()
		if err != nil {
			continue
		}
		var cfg ratelimiter.LimitConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		algoDist[string(cfg.Algorithm)]++
	}

	// Redis memory info (human-readable section from INFO memory).
	memInfo, _ := h.rdb.Info(r.Context(), "memory").Result()

	// Circuit breaker states: "closed" | "half-open" | "open"
	cbStates := make(map[string]string, len(h.breakers))
	for name, cb := range h.breakers {
		cbStates[name] = cb.State().String()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rate_limit_keys":        rlKeys,
		"override_keys":          len(algoDist),
		"algorithm_distribution": algoDist,
		"redis_memory":           memInfo,
		"circuit_breakers":       cbStates,
	})
}

// limitKey extracts the rate-limit key from the wildcard segment of the URL.
// Route: /admin/limits/* → key = everything after /admin/limits/
// Keys contain colons and forward slashes (e.g. rate-limit:user:route) so
// a wildcard segment is necessary instead of a named path parameter.
func limitKey(r *http.Request) string {
	return strings.TrimPrefix(r.URL.Path, "/admin/limits/")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
