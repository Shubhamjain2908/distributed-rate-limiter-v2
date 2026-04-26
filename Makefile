.PHONY: test race bench wire-measure integration trip-test

# Unit + integration coverage for default CI (no integration tag, no docker for integration)
test:
	go test ./...

race:
	go test -race ./...

# Docker required
integration:
	go test -tags=integration -race ./...

# Microbenchmarks (miniredis + in-process token bucket; wire-Redis: see README)
bench:
	go test -bench=. -benchmem -run=^$ ./internal/limiter/... ./internal/redis/...

# Real Redis on REDIS_BENCH_ADDR — same commands used for the wire row in README
wire-measure:
	@if [ -z "$$REDIS_BENCH_ADDR" ]; then echo "Usage: REDIS_BENCH_ADDR=127.0.0.1:6379 make wire-measure"; exit 1; fi
	go test -bench=BenchmarkRedisLimiter_Wire_Cost1 -benchtime=2s -count=5 -run=^$ ./internal/redis/...
	go test -v -run TestWireAllow_MeasureOneClient -count=1 ./internal/redis/...

# Requires Docker, docker compose, and stack reachable at TRIPTEST_BASE_URL
trip-test:
	@bash ./scripts/trip-test.sh
