local limit    = tonumber(ARGV[1])
local ttl      = tonumber(ARGV[2])
local pattern  = ARGV[3]
local workerKey= KEYS[1]

-- Sum all existing counters for this host (all workers)
local total = 0
local keys = redis.call("KEYS", pattern)
for _, k in ipairs(keys) do
  local v = tonumber(redis.call("GET", k) or "0")
  total = total + v
end

-- Enforce global limit
if limit > 0 and total >= limit then
  return {0, total}
end

-- Increment this worker's counter
local newVal = redis.call("INCR", workerKey)

-- Apply safety TTL for this worker's key, if requested
if ttl > 0 then
  redis.call("EXPIRE", workerKey, ttl)
end

return {1, total + 1}
