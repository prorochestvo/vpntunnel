# httpproxy — forward HTTP proxy over WireGuard
#
# Quick start:
#   1. Download a wg-quick .conf from your VPN provider.
#   2. mv ~/Downloads/<name>.conf ./configs/tunnels/
#   3. Edit configs/proxy.json: set upstream.configs and upstream.active.
#   4. make run
#
# NOTE: make run requires configs/proxy.json to be populated with at least
# one .conf path under upstream.configs. Out-of-the-box it will exit with
# a validation error — this is expected; configure it first.
#
# Docker targets (local dev only; CI builds + pushes to GHCR):
#   docker-build  — build the production image tagged httpproxy:dev.
#   docker-run    — run an existing local image, mounting ./configs/ and ./logs/.
#                   Does NOT build — call `make docker-build` first (or chain:
#                   `make docker-build docker-run`).
#                   NOTE: ./logs/ must be writable by UID 65532 (distroless nonroot).
#                   Run once: sudo chown 65532:65532 ./logs/

IMAGE_TAG ?= dev

.PHONY: build build-httpproxy run test lint format clean docker-build docker-run

build: format
	CGO_ENABLED=0 go build -o ./build/httpproxy ./cmd/httpproxy/

run: build
	CGO_ENABLED=0 go run ./cmd/httpproxy -config ./configs/proxy.json

test: lint
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi
	CGO_ENABLED=0 go vet ./...
	go test -race ./...

lint:
	CGO_ENABLED=0 go vet ./...
	@echo "checking forbidden imports (none banned in v3)"

format:
	go fmt ./...

# clean removes build artifacts. The bootstrap-mullvad entries are kept as
# belt-and-suspenders against accidental bare `go build` runs in the future;
# the binary has been removed from the project.
clean:
	rm -rf ./build ./tmp/*.tmp
	rm -f ./bootstrap-mullvad ./httpproxy ./build/bootstrap-mullvad ./build/httpproxy

# docker-build builds the production image locally and tags it as httpproxy:dev.
# CI builds + pushes; this target is for local smoke only.
docker-build:
	docker build -f configs/httpproxy.distroless.go1.26.dockerfile -t httpproxy:dev .

# docker-run runs a container from an existing local image with the operator's config
# tree mounted read-only and a writable logs/ volume. It does NOT build the image first
# — call `make docker-build` before this if you need a fresh local build, then
# `make docker-run` (or chain: `make docker-build docker-run`).
# Override the image tag with IMAGE_TAG=1.2.3 make docker-run — the image must already
# exist locally (pulled or tagged); this target does not pull from a registry.
# NOTE: ./logs/ must be writable by UID 65532 (distroless nonroot).
# Run once: sudo chown 65532:65532 ./logs/
docker-run:
	mkdir -p ./logs
	docker run --rm \
		-p 127.0.0.1:1701:1701 \
		-p 127.0.0.1:8081:8081 \
		-v $(PWD)/configs:/etc/httpproxy:ro \
		-v $(PWD)/logs:/var/log/httpproxy \
		httpproxy:$(IMAGE_TAG)


# init seeds the server's /opt/httpproxy/ with the operator-owned files
# (runtime config + WireGuard .conf + bearer-auth token). compose.yml is NOT
# shipped here — it is rendered by the deploy workflow on every run from
# configs/compose.yml in this repo, so the on-host file is a deploy-managed
# artifact, not operator-managed.
#
# Two proxy config files live in this repo:
#   configs/proxy.example.json — server template, binds on 0.0.0.0 so Docker's
#                                bridge port-forward can reach the listener.
#                                Shipped by `make init` and renamed to
#                                proxy.json on the server.
#   configs/proxy.json         — local-only, gitignored, binds on 127.0.0.1
#                                for `make run`. Each operator keeps their own.
#
# The bearer-auth token is generated ON THE SERVER (the secret never leaves
# the host). The openssl-or-write block is idempotent: if the token file
# already exists and is non-empty, it is left alone so active clients keep
# working. Delete the file on the server first to force a rotation.
#
# File ownership and mode are deliberately not touched here — the operator
# manages permissions on /opt/httpproxy/configs/{auth,tunnels} out of band.
init:
	scp ./configs/proxy.example.json be-happy.kz:/opt/httpproxy/configs/proxy.json
	scp ./configs/tunnels/mullvad-ch-zrh-wg-001.conf be-happy.kz:/opt/httpproxy/configs/tunnels/mullvad-ch-zrh-wg-001.conf
	ssh be-happy.kz '[ -s /opt/httpproxy/configs/auth/token ] || openssl rand -base64 128 | tr -d "\n" > /opt/httpproxy/configs/auth/token'