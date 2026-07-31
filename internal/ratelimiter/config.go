package ratelimiter

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// LimitConfig is the fully resolved rate-limit configuration for one request.
type LimitConfig struct {
	Algorithm Algorithm `json:"algorithm"`
	// Token Bucket
	Capacity   int64   `json:"capacity"`   // max tokens (also queue depth for Leaky Bucket)
	RefillRate float64 `json:"refillRate"` // tokens per second
	// Sliding / Fixed Window
	Limit      int64 `json:"limit"`      // max requests in the window
	WindowSecs int64 `json:"windowSecs"` // window size in seconds
	// Leaky Bucket
	Rate float64 `json:"rate"` // drain rate in requests per second
}

// planAlgoConfigs defines the four algorithms' parameters for each plan tier.
// JWT algorithm claim lets a user pick which algorithm applies to their traffic.
var planAlgoConfigs = map[string]map[Algorithm]LimitConfig{
	"free": {
		AlgorithmTokenBucket:   {Algorithm: AlgorithmTokenBucket, Capacity: 100, RefillRate: 1.67},
		AlgorithmSlidingWindow: {Algorithm: AlgorithmSlidingWindow, Limit: 100, WindowSecs: 60},
		AlgorithmFixedWindow:   {Algorithm: AlgorithmFixedWindow, Limit: 100, WindowSecs: 60},
		AlgorithmLeakyBucket:   {Algorithm: AlgorithmLeakyBucket, Rate: 1.67, Capacity: 10},
	},
	"pro": {
		AlgorithmTokenBucket:   {Algorithm: AlgorithmTokenBucket, Capacity: 500, RefillRate: 8.33},
		AlgorithmSlidingWindow: {Algorithm: AlgorithmSlidingWindow, Limit: 500, WindowSecs: 60},
		AlgorithmFixedWindow:   {Algorithm: AlgorithmFixedWindow, Limit: 500, WindowSecs: 60},
		AlgorithmLeakyBucket:   {Algorithm: AlgorithmLeakyBucket, Rate: 8.33, Capacity: 50},
	},
	"enterprise": {
		AlgorithmTokenBucket:   {Algorithm: AlgorithmTokenBucket, Capacity: 2000, RefillRate: 33.33},
		AlgorithmSlidingWindow: {Algorithm: AlgorithmSlidingWindow, Limit: 2000, WindowSecs: 60},
		AlgorithmFixedWindow:   {Algorithm: AlgorithmFixedWindow, Limit: 2000, WindowSecs: 60},
		AlgorithmLeakyBucket:   {Algorithm: AlgorithmLeakyBucket, Rate: 33.33, Capacity: 200},
	},
}

var globalDefault = LimitConfig{
	Algorithm:  AlgorithmTokenBucket,
	Capacity:   50,
	RefillRate: 0.83, // ~50/min
}

// localEntry is one item in the in-process config cache.
type localEntry struct {
	cfg LimitConfig
	ok  bool // false → key has no Redis override
	exp time.Time
}

// ConfigResolver resolves the applicable LimitConfig and Redis key for a
// (userID, route, plan, algoHint) tuple. Resolution order:
//  1. Per-key Redis override (written by admin API)
//  2. Plan + algorithm hint from JWT claim
//  3. Plan default algorithm (TOKEN_BUCKET)
//  4. Global default
//
// Results are cached in-process for cacheTTL to avoid a Redis round-trip on
// every request. The cache is flushed by FlushCache(), called when
// POST /admin/config/reload is received or a Redis pub/sub flush event arrives.
type ConfigResolver struct {
	rdb      *redis.Client
	cacheTTL time.Duration
	mu       sync.RWMutex
	local    map[string]localEntry
}

// NewConfigResolver creates a ConfigResolver with an empty local cache.
func NewConfigResolver(rdb *redis.Client, cacheTTL time.Duration) *ConfigResolver {
	return &ConfigResolver{
		rdb:      rdb,
		cacheTTL: cacheTTL,
		local:    make(map[string]localEntry),
	}
}

// Resolve returns the (LimitConfig, redisKey) pair for this request.
// algoHint is the algorithm claim from the JWT; pass "" to use the plan default.
func (cr *ConfigResolver) Resolve(ctx context.Context, userID, route, plan, algoHint string) (LimitConfig, string) {
	rateLimitKey := fmt.Sprintf("rate-limit:%s:%s", userID, route)

	// 1. Per-key Redis override (e.g. set via admin API for a VIP user)
	if cfg, ok := cr.loadOverride(ctx, rateLimitKey); ok {
		return cfg, rateLimitKey
	}

	// 2. Plan-based config, optionally with the JWT algorithm hint
	if planCfgs, ok := planAlgoConfigs[plan]; ok {
		if algoHint != "" {
			if cfg, ok := planCfgs[Algorithm(algoHint)]; ok {
				return cfg, rateLimitKey
			}
		}
		if cfg, ok := planCfgs[AlgorithmTokenBucket]; ok {
			return cfg, rateLimitKey
		}
	}

	// 3. Global default
	return globalDefault, fmt.Sprintf("rate-limit:global:%s", route)
}

// SetOverride stores a runtime config override for key. Used by admin API.
func (cr *ConfigResolver) SetOverride(ctx context.Context, key string, cfg LimitConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	if err := cr.rdb.Set(ctx, "rl-config:"+key, data, cr.cacheTTL*10).Err(); err != nil {
		return err
	}
	// Evict local cache entry so the next request re-reads from Redis.
	cr.mu.Lock()
	delete(cr.local, key)
	cr.mu.Unlock()
	return nil
}

// GetOverride reads a per-key config override directly from Redis (bypasses
// local cache so admin reads are always fresh).
func (cr *ConfigResolver) GetOverride(ctx context.Context, key string) (LimitConfig, bool) {
	data, err := cr.rdb.Get(ctx, "rl-config:"+key).Bytes()
	if err != nil {
		return LimitConfig{}, false
	}
	var cfg LimitConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return LimitConfig{}, false
	}
	return cfg, true
}

// DeleteOverride removes a per-key override, falling back to plan defaults on
// the next request.
func (cr *ConfigResolver) DeleteOverride(ctx context.Context, key string) error {
	cr.mu.Lock()
	delete(cr.local, key)
	cr.mu.Unlock()
	return cr.rdb.Del(ctx, "rl-config:"+key).Err()
}

// FlushCache clears the entire in-process cache. Called when
// POST /admin/config/reload is received, and by the Redis pub/sub subscriber
// so all gateway instances flush when any one of them reloads.
func (cr *ConfigResolver) FlushCache() {
	cr.mu.Lock()
	cr.local = make(map[string]localEntry)
	cr.mu.Unlock()
}

// loadOverride checks the local cache first, then Redis.
func (cr *ConfigResolver) loadOverride(ctx context.Context, key string) (LimitConfig, bool) {
	cr.mu.RLock()
	if e, found := cr.local[key]; found && time.Now().Before(e.exp) {
		cr.mu.RUnlock()
		return e.cfg, e.ok
	}
	cr.mu.RUnlock()

	// Cache miss — query Redis.
	data, err := cr.rdb.Get(ctx, "rl-config:"+key).Bytes()
	entry := localEntry{exp: time.Now().Add(cr.cacheTTL)}
	if err == nil {
		var cfg LimitConfig
		if json.Unmarshal(data, &cfg) == nil {
			entry.cfg = cfg
			entry.ok = true
		}
	}

	cr.mu.Lock()
	cr.local[key] = entry
	cr.mu.Unlock()

	return entry.cfg, entry.ok
}
