package ratelimiter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// LimitConfig is the fully resolved rate-limit configuration for one request.
// It carries everything the Lua script needs: which algorithm to run and
// the parameters specific to that algorithm.
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

// defaultPlanLimits maps JWT plan claims to baseline token-bucket configs.
// Phase 3 reads the plan from the JWT; Phase 2 defaults every request to "free".
var defaultPlanLimits = map[string]LimitConfig{
	"free":       {Algorithm: AlgorithmTokenBucket, Capacity: 100, RefillRate: 1.67},   // ~100/min
	"pro":        {Algorithm: AlgorithmTokenBucket, Capacity: 500, RefillRate: 8.33},   // ~500/min
	"enterprise": {Algorithm: AlgorithmTokenBucket, Capacity: 2000, RefillRate: 33.33}, // ~2000/min
}

var globalDefault = LimitConfig{
	Algorithm:  AlgorithmTokenBucket,
	Capacity:   50,
	RefillRate: 0.83, // ~50/min
}

// ConfigResolver resolves the applicable LimitConfig and Redis key for a
// (userID, route, plan) triple. Resolution order (highest specificity wins):
//  1. Per-key Redis override — written by the admin API (Phase 3)
//  2. Plan-based default     — from JWT plan claim (Phase 3), "free" in Phase 2
//  3. Global default
type ConfigResolver struct {
	rdb      *redis.Client
	cacheTTL time.Duration
}

// NewConfigResolver creates a ConfigResolver. cacheTTL is the lifetime of
// per-key overrides stored by the admin API.
func NewConfigResolver(rdb *redis.Client, cacheTTL time.Duration) *ConfigResolver {
	return &ConfigResolver{rdb: rdb, cacheTTL: cacheTTL}
}

// Resolve returns the (LimitConfig, redisKey) pair for this request.
// redisKey is passed as KEYS[1] to the Lua script; it encodes both the subject
// (user/IP) and the resource (route) so each (subject, route) pair gets its
// own independent counter.
func (cr *ConfigResolver) Resolve(ctx context.Context, userID, route, plan string) (LimitConfig, string) {
	rateLimitKey := fmt.Sprintf("rate-limit:%s:%s", userID, route)

	if cfg, ok := cr.loadOverride(ctx, rateLimitKey); ok {
		return cfg, rateLimitKey
	}

	if cfg, ok := defaultPlanLimits[plan]; ok {
		return cfg, rateLimitKey
	}

	return globalDefault, fmt.Sprintf("rate-limit:global:%s", route)
}

// SetOverride stores a runtime config override for key. The admin API (Phase 3)
// calls this to change a limit without restarting the gateway.
func (cr *ConfigResolver) SetOverride(ctx context.Context, key string, cfg LimitConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	return cr.rdb.Set(ctx, "rl-config:"+key, data, cr.cacheTTL).Err()
}

// DeleteOverride removes a per-key override, falling back to plan defaults.
func (cr *ConfigResolver) DeleteOverride(ctx context.Context, key string) error {
	return cr.rdb.Del(ctx, "rl-config:"+key).Err()
}

func (cr *ConfigResolver) loadOverride(ctx context.Context, key string) (LimitConfig, bool) {
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
