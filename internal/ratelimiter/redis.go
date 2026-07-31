package ratelimiter

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

//go:embed scripts/*.lua
var luaScripts embed.FS

// RedisClient wraps go-redis and pre-loads all four Lua scripts via SCRIPT LOAD
// at startup. Each request then uses EVALSHA (40-byte SHA) instead of
// resending the full script body, saving bandwidth on every hot-path call.
type RedisClient struct {
	rdb          *redis.Client
	scriptSHAs   map[Algorithm]string
	scriptBodies map[Algorithm]string
}

// NewRedisClient connects to Redis, confirms liveness with PING, then loads
// all four Lua scripts. Fails fast — if a script can't load no requests would
// succeed anyway, so it is better to crash at startup than silently at request time.
func NewRedisClient(ctx context.Context, addr, password string, db, poolSize int) (*RedisClient, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
		PoolSize: poolSize,
	})

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}

	rc := &RedisClient{
		rdb:          rdb,
		scriptSHAs:   make(map[Algorithm]string),
		scriptBodies: make(map[Algorithm]string),
	}

	scripts := map[Algorithm]string{
		AlgorithmTokenBucket:   "scripts/token_bucket.lua",
		AlgorithmSlidingWindow: "scripts/sliding_window.lua",
		AlgorithmFixedWindow:   "scripts/fixed_window.lua",
		AlgorithmLeakyBucket:   "scripts/leaky_bucket.lua",
	}

	for algo, path := range scripts {
		body, err := luaScripts.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		sha, err := rdb.ScriptLoad(ctx, string(body)).Result()
		if err != nil {
			return nil, fmt.Errorf("loading %s script: %w", algo, err)
		}
		rc.scriptSHAs[algo] = sha
		rc.scriptBodies[algo] = string(body)
	}

	return rc, nil
}

// EvalScript executes the pre-loaded Lua script for algo.
// On NOSCRIPT (Redis restarted and evicted the script cache), it falls back
// to Eval with the full body and reloads the SHA for subsequent calls.
func (rc *RedisClient) EvalScript(ctx context.Context, algo Algorithm, keys []string, args ...interface{}) (interface{}, error) {
	sha := rc.scriptSHAs[algo]
	res, err := rc.rdb.EvalSha(ctx, sha, keys, args...).Result()
	if err == nil {
		return res, nil
	}

	if strings.Contains(err.Error(), "NOSCRIPT") {
		body := rc.scriptBodies[algo]
		res, err = rc.rdb.Eval(ctx, body, keys, args...).Result()
		if err == nil {
			if newSHA, serr := rc.rdb.ScriptLoad(ctx, body).Result(); serr == nil {
				rc.scriptSHAs[algo] = newSHA
			}
		}
	}
	return res, err
}

// Client exposes the underlying go-redis client for use by ConfigResolver.
func (rc *RedisClient) Client() *redis.Client { return rc.rdb }

// Close releases the connection pool.
func (rc *RedisClient) Close() error { return rc.rdb.Close() }
