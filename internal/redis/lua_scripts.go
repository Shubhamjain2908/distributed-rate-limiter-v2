package redis

// TokenBucketScript is loaded with SCRIPT LOAD; the server returns a SHA1 used with EVALSHA.
// KEYS[1] = key, ARGV = capacity, refillPerSec, now (epoch seconds, fractional), cost.
// Returns {ok (0/1), remaining, retryAfterMs} where ok=1 if allowed.
const TokenBucketScript = `
local key = KEYS[1]
local capacity    = tonumber(ARGV[1])
local refill_rate = tonumber(ARGV[2])
local now         = tonumber(ARGV[3])
local requested   = tonumber(ARGV[4])

if capacity == nil or refill_rate == nil or now == nil or requested == nil then
  return {0, 0, 0}
end
if requested < 0 then
  requested = 0
end

local data = redis.call('HMGET', key, 'tokens', 'last_refill')
local tokens = tonumber(data[1])
local last   = tonumber(data[2])
if not tokens or not last then
  tokens = capacity
  last   = now
end

local elapsed = math.max(0, now - last)
if elapsed > 0 and refill_rate > 0 then
  tokens = math.min(capacity, tokens + elapsed * refill_rate)
end
last = now

local need = requested
if tokens + 0.000001 < need then
  local missing = need - tokens
  local waitSec = 0.0
  if refill_rate > 0 then
    waitSec = missing / refill_rate
  else
    waitSec = 3600
  end
  local retryMs = math.ceil(waitSec * 1000.0)
  if retryMs < 0 then
    retryMs = 0
  end
  local rem = math.floor(math.min(capacity, tokens))
  redis.call('HMSET', key, 'tokens', tostring(tokens), 'last_refill', tostring(last))
  redis.call('EXPIRE', key, 3600)
  return {0, rem, retryMs}
end

tokens = tokens - need
local rem = math.floor(math.min(capacity, tokens))
if rem < 0 then
  rem = 0
end
redis.call('HMSET', key, 'tokens', tostring(tokens), 'last_refill', tostring(last))
redis.call('EXPIRE', key, 3600)
return {1, rem, 0}
`
