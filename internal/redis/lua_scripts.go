local key         = KEYS[1]
local capacity    = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local now         = tonumber(ARGV[3])
local requested   = tonumber(ARGV[4])

-- 1. Fetch current state
local bucket = redis.call('HMGET', key, 'tokens', 'last_refill')
local tokens = tonumber(bucket[1]) or capacity
local last   = tonumber(bucket[2]) or now

-- 2. Refill tokens based on elapsed time
local elapsed = math.max(0, now - last)
tokens = math.min(capacity, tokens + elapsed * refill_rate)

-- 3. Check if allowed and deduct
local allowed = tokens >= requested
if allowed then 
    tokens = tokens - requested 
end

-- 4. Save state and set TTL
redis.call('HMSET', key, 'tokens', tokens, 'last_refill', now)
redis.call('EXPIRE', key, 3600)

-- 5. Return results to Go
return { allowed and 1 or 0, math.floor(tokens) }