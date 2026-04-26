#!/usr/bin/env bash
# Stops Redis, measures first-request fallback latency, drives the breaker to Open, restarts
# Redis, and verifies primary recovery (EVALSHA) after the 30s open + half-open probe.
#
# Env:
#   TRIPTEST_BASE_URL   default http://127.0.0.1:8080
#   TRIPTEST_COMPOSE   default: docker compose (or docker-compose)
#   TRIPTEST_FIRST_MAX_MS  first-response budget in ms (default 150; triptest machine dependent)

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
COMPOSE_FILE="${ROOT}/docker-compose.yml"

COMPOSE="${TRIPTEST_COMPOSE:-docker compose}"
if ! $COMPOSE -f "$COMPOSE_FILE" version &>/dev/null; then
  COMPOSE="docker-compose"
  if ! $COMPOSE -f "$COMPOSE_FILE" version &>/dev/null; then
    echo "trip-test: need a working 'docker compose' or 'docker-compose'" >&2
    exit 1
  fi
fi

BASE="${TRIPTEST_BASE_URL:-http://127.0.0.1:8080}"
MAX_MS="${TRIPTEST_FIRST_MAX_MS:-100}"
HDR=( -H "X-Client-ID: triptest-$(date +%s)" )
path="${BASE}/"

command -v curl &>/dev/null || { echo "trip-test: curl required" >&2; exit 1; }
command -v python3 &>/dev/null || { echo "trip-test: python3 required" >&2; exit 1; }

# Bring stack up (idempotent)
$COMPOSE -f "$COMPOSE_FILE" up -d app redis
echo "=== wait for healthz ${BASE}/healthz"
ok=0
for _ in $(seq 1 60); do
  if curl -sf -m 2 -o /dev/null "${BASE}/healthz"; then
    ok=1
    break
  fi
  sleep 1
done
if [ "$ok" -ne 1 ]; then
  echo "trip-test: app not healthy" >&2
  exit 1
fi

echo "=== stop Redis (primary down) ==="
$COMPOSE -f "$COMPOSE_FILE" stop redis

echo "=== first GET after stop (local fallback, wall time < ${MAX_MS}ms) ==="
t0_ms="$(python3 -c 'import time; print(int(time.time()*1000))')"
code="$(curl -sS -o /dev/null -w '%{http_code}' "${HDR[@]}" -m 3 --get "$path" || true)"
t1_ms="$(python3 -c 'import time; print(int(time.time()*1000))')"
elapsed_ms=$((t1_ms - t0_ms))
[ "$code" = "200" ] || { echo "trip-test: want 200, got $code" >&2; exit 1; }
echo "    http $code, wall ${elapsed_ms}ms"
[ "$elapsed_ms" -le "$MAX_MS" ] || {
  echo "trip-test: ${elapsed_ms}ms > ${MAX_MS}ms — raise TRIPTEST_FIRST_MAX_MS if on a slow host" >&2
  exit 1
}
algo1="$(curl -sS -D- -o /dev/null "${HDR[@]}" -m 3 --get "$path" | tr -d '\r' | awk 'tolower($0) ~ /^x-rate-limit-algorithm:/{gsub(/^[ \t]+/,"", $2); print tolower($2); exit}')"
# Must not be only redis; usually "in_memory" (LocalLimiter)
if [ -z "$algo1" ]; then
  echo "trip-test: missing X-Rate-Limit-Algorithm" >&2
  exit 1
fi
if [ "$algo1" = "redis" ]; then
  echo "trip-test: first response still on redis (Redis still reachable?)" >&2
  exit 1
fi
echo "    algorithm: $algo1 (fallback path expected)"

# Four more failed primaries (window already has 1) → trip Open
for _ in 2 3 4 5; do
  curl -sS -o /dev/null -m 3 "${HDR[@]}" --get "$path" || true
done
# Gauge poll is 2s; give Prometheus /metrics a moment
sleep 3
echo "=== /metrics: circuit should be open ==="
metrics="$(curl -sS -m 3 "${BASE}/metrics")"
if ! echo "$metrics" | grep -F 'ratelimiter_circuit_state{state="open"}' | head -1 | grep -E -q ' 1(\\.0+)?$'; then
  echo "trip-test: want ratelimiter_circuit_state{state=\"open\"} 1" >&2
  echo "$metrics" | grep ratelimiter_circuit_state || true
  exit 1
fi

echo "=== start Redis (soon after trip; must be up before half-open probe) ==="
$COMPOSE -f "$COMPOSE_FILE" start redis
# Wait for "Ready to accept connections"
sleep 4
echo "    then ~32s for openDuration (30s) before half-open probe on next Route() …"
sleep 32

echo "=== expect primary path: header contains 'redis' ==="
ok_redis=0
for _ in $(seq 1 30); do
  a="$(curl -sS -D- -o /dev/null "${HDR[@]}" -m 3 --get "$path" | tr -d '\r' | awk 'tolower($0) ~ /^x-rate-limit-algorithm:/{gsub(/^[ \t]+/,"", $2); print tolower($2); exit}')"
  if [ -n "$a" ] && echo "$a" | grep -qF 'redis'; then
    ok_redis=1
    break
  fi
  sleep 1
done
[ "$ok_redis" = 1 ] || {
  echo "trip-test: recovery: expected X-Rate-Limit-Algorithm to show redis" >&2
  curl -sS -D- -o /dev/null "${HDR[@]}" --get "$path" | tr -d '\r' | grep -i x-rate || true
  exit 1
}

echo "OK: fast fallback, breaker open, Redis primary restored"
