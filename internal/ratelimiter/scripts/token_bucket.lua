-- Token Bucket rate limiter
--
-- KEYS[1]  bucket key  (rate-limit:{userId}:{route})
-- ARGV[1]  capacity    max tokens the bucket can hold (burst limit)
-- ARGV[2]  refill_rate tokens added per second (float)
-- ARGV[3]  cost        tokens to consume this request
--
-- Returns {allowed, remaining, wait_ms}
--   allowed   1 = request permitted, 0 = rejected
--   remaining tokens left after this request (floored)
--   wait_ms   ms until `cost` tokens are available (0 when allowed)
--
-- redis.call('TIME') is used so the refill calculation uses the Redis server
-- clock, not individual gateway clocks which may drift apart.

local key         = KEYS[1]
local capacity    = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local cost        = tonumber(ARGV[3])

local t      = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local data        = redis.call('HMGET', key, 'tokens', 'last_refill')
local tokens      = tonumber(data[1])
local last_refill = tonumber(data[2])

if not tokens then
    tokens      = capacity
    last_refill = now_ms
end

local elapsed_ms = math.max(0, now_ms - last_refill)
tokens = math.min(capacity, tokens + elapsed_ms * refill_rate / 1000)

if tokens >= cost then
    tokens = tokens - cost
    -- TTL: time to fully drain the bucket from capacity, plus a 60s buffer
    local ttl_ms = math.ceil(capacity / refill_rate * 1000) + 60000
    redis.call('HMSET', key, 'tokens', tokens, 'last_refill', now_ms)
    redis.call('PEXPIRE', key, ttl_ms)
    return {1, math.floor(tokens), 0}
end

local deficit = cost - tokens
local wait_ms = math.ceil(deficit * 1000 / refill_rate)
return {0, math.floor(tokens), wait_ms}
