-- Fixed Window rate limiter
--
-- KEYS[1]  base key  (rate-limit:{userId}:{route})
-- ARGV[1]  limit        max requests per window
-- ARGV[2]  window_secs  window size in seconds
-- ARGV[3]  cost
--
-- Returns {allowed, remaining, wait_ms}
--
-- The actual counter key is derived: KEYS[1]:w{window_number}, where
-- window_number = floor(now / window_secs).  Each window gets a fresh
-- counter that expires one second after the window closes.
--
-- BOUNDARY-BURST VULNERABILITY (intentional — documented for comparison):
-- Sending `limit` requests at t=window_end-1s and `limit` at t=window_start+1s
-- allows 2*limit requests in 2 seconds.  Token Bucket and Sliding Window
-- both prevent this; Fixed Window does not.  Demonstrated in
-- tests/concurrency/window_burst_test.go.

local base_key    = KEYS[1]
local limit       = tonumber(ARGV[1])
local window_secs = tonumber(ARGV[2])
local cost        = tonumber(ARGV[3])

local t          = redis.call('TIME')
local now_secs   = tonumber(t[1])
local window_num = math.floor(now_secs / window_secs)
local key        = base_key .. ':w' .. tostring(window_num)

local current = tonumber(redis.call('GET', key)) or 0

if current + cost <= limit then
    redis.call('INCRBY', key, cost)
    -- TTL: remainder of this window plus 1s buffer so the key outlives the window
    local ttl = window_secs - (now_secs % window_secs) + 1
    redis.call('EXPIRE', key, ttl)
    return {1, limit - current - cost, 0}
end

local wait_ms = (window_secs - (now_secs % window_secs)) * 1000
return {0, 0, wait_ms}
