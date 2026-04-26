.PHONY: test race bench integration trip-test

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

# Requires Docker, docker compose, and stack reachable at TRIPTEST_BASE_URL
trip-test:
	@bash ./scripts/trip-test.sh
