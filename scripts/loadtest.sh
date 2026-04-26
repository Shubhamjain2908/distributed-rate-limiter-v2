#!/usr/bin/env bash
# Generate HTTP load so Prometheus → Grafana show rate limiter + Redis SLOs.
# With docker compose, the app is http://127.0.0.1:8080
#
# Usage:
#   chmod +x scripts/loadtest.sh
#   ./scripts/loadtest.sh
#   ./scripts/loadtest.sh 'http://127.0.0.1:8080' 40 5000
#     args: BASE (no trailing slash)  CONCURRENT  TOTAL_REQUESTS
#
# Optional: go install github.com/rakyll/hey@latest
#   hey -n 100000 -c 100 -H "X-Client-ID: demo" 'http://127.0.0.1:8080/'

set -euo pipefail
H="${1:-http://127.0.0.1:8080}"
H="${H%/}"
P="${2:-30}"
N="${3:-5000}"
command -v curl &>/dev/null || { echo "need curl" >&2; exit 1; }
echo "=== $N requests, $P workers → $H (X-Client-ID: load-0..49, mixed paths) ==="
seq 1 "$N" | xargs -P"$P" -I{} sh -c '
  H="$1"
  I="$2"
  case $((I % 3)) in
    0) U="$H/" ;;
    1) U="$H/search/map" ;;
    *) U="$H/search/basic" ;;
  esac
  C=$((I % 50))
  curl -sS -o /dev/null -H "X-Client-ID: load-$C" -m 10 --get "$U" || true
' _ "$H" {}
echo
echo "=== done. (Prometheus scrape interval: 5s; give Grafana a short time range and refresh.)"
