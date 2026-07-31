-- Leaky Bucket rate limiter
--
-- KEYS[1]  bucket key  (rate-limit:{userId}:{route})
-- ARGV[1]  rate      drain rate in requests per second
-- ARGV[2]  capacity  max queue depth (burst protection)
-- ARGV[3]  cost
--
-- Returns {allowed, remaining_capacity, wait_ms}
--   allowed=1, wait_ms=0   → serve immediately (queue was empty)
--   allowed=1, wait_ms>0   → accepted into queue; caller should delay
--                             the downstream call by wait_ms milliseconds
--   allowed=0              → queue full; reject immediately
--
-- Implementation: "virtual queue" — tracks queue_end (the ms timestamp when
-- the last queued request will be processed) and queue_size.  New requests
-- append to the end of the virtual queue.  If queue_end <= now the queue
-- has fully drained and is reset.

local key      = KEYS[1]
local rate     = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local cost     = tonumber(ARGV[3])

local t      = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local slot_ms = math.floor(1000 / rate)

local data       = redis.call('HMGET', key, 'queue_end', 'queue_size')
local queue_end  = tonumber(data[1]) or now_ms
local queue_size = tonumber(data[2]) or 0

if queue_end <= now_ms then
    queue_end  = now_ms
    queue_size = 0
end

local new_size = queue_size + cost
if new_size > capacity then
    return {0, 0, queue_end - now_ms}
end

local serve_at = queue_end
queue_end      = queue_end + slot_ms * cost

redis.call('HMSET', key, 'queue_end', queue_end, 'queue_size', new_size)
local ttl_ms = math.ceil(capacity / rate * 1000) + 60000
redis.call('PEXPIRE', key, ttl_ms)

return {1, capacity - new_size, math.max(0, serve_at - now_ms)}
