-- Sliding Window rate limiter
--
-- KEYS[1]  sorted-set key  (rate-limit:{userId}:{route})
-- ARGV[1]  limit        max requests in the window
-- ARGV[2]  window_secs  window size in seconds
-- ARGV[3]  cost         request slots to consume
--
-- Returns {allowed, remaining, wait_ms}
--
-- Each request is stored as a sorted-set member with score = timestamp_ms.
-- Expired members (older than the window) are pruned on every call so the
-- set size is bounded to at most `limit` members at any time.
--
-- Member uniqueness: seconds + microseconds + loop index ensures concurrent
-- requests within the same millisecond produce distinct entries without
-- using math.random (which is non-deterministic and unsuitable for replicated
-- Lua execution).
--
-- Boundary-burst immunity: there is no reset boundary. The window rolls
-- continuously, so sending limit requests at 11:59:59 and limit more at
-- 12:00:01 correctly results in rejection — unlike Fixed Window.

local key         = KEYS[1]
local limit       = tonumber(ARGV[1])
local window_secs = tonumber(ARGV[2])
local cost        = tonumber(ARGV[3])

local t         = redis.call('TIME')
local now_ms    = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local window_ms = window_secs * 1000

redis.call('ZREMRANGEBYSCORE', key, '-inf', now_ms - window_ms)

local count = tonumber(redis.call('ZCARD', key))

if count + cost <= limit then
    for i = 1, cost do
        local member = t[1] .. t[2] .. tostring(i)
        redis.call('ZADD', key, now_ms, member)
    end
    redis.call('PEXPIRE', key, window_ms + 1000)
    return {1, limit - count - cost, 0}
end

local oldest  = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local wait_ms = 1000
if #oldest >= 2 then
    wait_ms = math.max(0, tonumber(oldest[2]) + window_ms - now_ms)
end
return {0, math.max(0, limit - count), wait_ms}
