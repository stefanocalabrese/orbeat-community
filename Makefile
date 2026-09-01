.PHONY: test vet build up down smoke smoke-remote clean marketplace

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
	CGO_ENABLED=0 go build -o bin/orbeat-sync ./cmd/orbeat-sync

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

clean:
	rm -rf bin

marketplace:
	go run ./cmd/marketplacegen -out marketplace -gateway-url $${ORBEAT_GATEWAY_RESOURCE_URL:-http://localhost:8090}
