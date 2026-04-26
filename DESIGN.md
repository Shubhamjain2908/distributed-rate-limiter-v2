# Design notes

## Why **Lua in one EVAL/EVALSHA** instead of **Redis transactions (MULTI/EXEC, WATCH)**

- **Read–modify–write in one place.** The limiter’s token bucket is “load hash → compute refill and deduct → write hash and TTL.” A plain **MULTI/EXEC** can batch commands, but **WATCH/optimistic** retries add tail latency and a thundering error path under contention, while a pure **script** runs **atomically in Redis** in a single evaluation without cross-command races from other clients.
- **No lost updates between HGET* and HSET** without a loop. A naive transaction sequence (GET fields + conditional SET in separate command batches) is not a single **atomic** decision the way a Lua **EVAL** is, unless you wrap a server-side `FUNCTION` or script—same class of solution.
- **TTL + hash** in the same turn: the script can `EXPIRE` after `HMSET` in one go; wiring that as multiple separate commands in a `MULTI` is possible but *still* bakes in client round-trips and layout complexity, with no real benefit over a script.
- **Trade-off:** you accept **Redis 7 single-threaded execution** per key shard and must keep the script O(1). That is usually cheaper than an application-side retry loop with WATCH for hot keys.

## **EVAL** vs **EVALSHA**

| | `EVAL` (string body each time) | `EVALSHA` (SHA1) |
|---|-------------------------------|------------------|
| **Pros** | Simple; no script load step | **Less bandwidth** to Redis; server caches bytecode by digest |
| **Cons** | Sends full script on every call | **NOSCRIPT** after `SCRIPT FLUSH` / new server: must `SCRIPT LOAD` and retry (handled in this repo) |
| **We use** | N/A on the hot path | `SCRIPT LOAD` on startup, then `EvalSha` + one-shot reload on NOSCRIPT |

*Fallback path:* if the cluster evicts the script, one extra round trip reloads; steady state is EVALSHA-only.

## What breaks (or gets painful) around **~1M+ requests per minute** (~17k RPS) and beyond

Exact ceilings are deployment-specific; these are the first pressure points we design around:

1. **Redis single command thread (per shard).** `EVALSHA` time + queueing: more than **~1 core** of work per key/shard. Scale **horizontal** (many Redis nodes / cluster slots) and **separate** hot `clientId` keys, not a single hot key, if you need linear growth.
2. **Network + syscall overhead** between app and Redis: **one** RTT per `Allow` on the happy path. Co-locating, connection pooling, and avoiding N+1 patterns in higher layers still leaves **one** Redis hop per check unless you **batch** decisions in the app (out of scope for a per-HTTP `Allow`).
3. **Lua work must stay O(1).** A script that does extra logging or unbounded work per call will show up in **p99** and trip the **latency** breaker.
4. **Key explosion:** `1M+ req/min` *spread across* many `X-Client-ID` values = many hash keys, **memory** growth in Redis; **tune** `EXPIRE` and idle eviction policies.
5. **Application CPU:** serialization of `go-redis`, Prometheus, HTTP stack — on a **2-CPU** node, the **in-memory** fallback (no I/O) can outrun Redis **per core**; that is expected and is why the **1M+ req/min** SLO is scoped to the **local** path in the README, not the Redis path.

6. **Circuit breaker** opens after sustained primary failures, shifting load to the **local** limiter — you trade **strong global fairness** (Redis) for **process-local** behavior under blast radius control.

## Related code

- Script: `internal/redis/lua_scripts.go`
- Transport + reload: `internal/redis/redis_limiter.go`
- Composite: `cmd/server` (`primaryThenLocal` + `circuitbreaker`)
- Metrics: `internal/metrics/metrics.go` (Redis p99, circuit, fallback, error budget)
