.PHONY: test vet build up down smoke smoke-remote smoke-prod clean marketplace \
        ci ci-go ci-node ci-security ci-fast

test:
	go test ./... -race

vet:
	go vet ./...

# CGO_ENABLED=0 matches the container images (static distroless binaries), so a
# dev `make build` produces the same artifact the Dockerfiles do.
build:
	CGO_ENABLED=0 go build -o bin/orbeat-api ./cmd/api
	CGO_ENABLED=0 go build -o bin/orbeat-gateway ./cmd/gateway
	CGO_ENABLED=0 go build -o bin/orbeat-portal ./cmd/portal
	CGO_ENABLED=0 go build -o bin/orbeat-sync ./cmd/sync

up:
	docker compose -f deploy/docker-compose.yml up --build -d

# --profile observability so a `down` after `--profile observability up` also
# removes the collector and Jaeger. WITHOUT the flag, compose removes only the
# non-profiled services, leaves the profiled ones RUNNING, fails to delete the
# network, and still EXITS 0 — measured. The flag is a no-op when the profile
# never started.
down:
	docker compose -f deploy/docker-compose.yml --profile observability down -v

smoke:
	./scripts/smoke.sh

# Remote-target publisher gate: exercises the git:// push path (failure→recovery)
# that the local-target `smoke` never runs. See scripts/smoke-remote.sh.
smoke-remote:
	./scripts/smoke-remote.sh

# Local production-stack smoke: prod compose + build-from-source + Caddy
# internal TLS + *.localhost (no ACME, no /etc/hosts). See scripts/smoke-prod.sh.
smoke-prod:
	./scripts/smoke-prod.sh

# ── local CI ──────────────────────────────────────────────────────────────────
# Run the .github/workflows/ci.yml matrix on this machine, costing zero Actions
# minutes. scripts/ci-local.sh documents every deliberate divergence from CI and
# prints the fidelity gaps (node major, Docker runtime) in its summary — a local
# green is real signal, but it is NOT the CI gate.

# All 7 jobs, stopping at the first failure. KEEP_GOING=1 runs them all.
ci:
	./scripts/ci-local.sh

# The two jobs that need no Docker at all (~15s warm) — the pre-commit loop.
# `go` is deliberately NOT in here: its testcontainers suite needs Docker.
ci-fast:
	./scripts/ci-local.sh node security

ci-go:
	./scripts/ci-local.sh go

ci-node:
	./scripts/ci-local.sh node

# The only job that can go red with an UNCHANGED tree, because dependency risk
# accrues on wall-clock time rather than on commits (v1.21.0's nine-day outage
# was found by exactly this). Run it on a schedule, not when you remember.
ci-security:
	./scripts/ci-local.sh security

clean:
	rm -rf bin

marketplace:
	go run ./cmd/marketplacegen -out marketplace -gateway-url $${ORBEAT_GATEWAY_RESOURCE_URL:-http://localhost:8090}
