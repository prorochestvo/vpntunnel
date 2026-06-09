# stage 1 — builder
FROM golang:1.26-alpine AS builder

WORKDIR /src

# copy module files first so `go mod download` is its own cached layer;
# a pure source change will not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags='-s -w' \
    -o /out/httpproxy \
    ./cmd/httpproxy/

# stage 2 — runtime (distroless; no shell, no package manager)
FROM gcr.io/distroless/static-debian12:nonroot

# COPY runs as root so BuildKit can write to /; USER drops to nonroot after.
# --chown ensures the binary is owned by the nonroot user inside the image.
COPY --chown=nonroot:nonroot --from=builder /out/httpproxy /httpproxy

# UID 65532 is the nonroot user baked into this image.
USER nonroot

# 1701 — forward proxy port (default; overridable via listen in proxy.json)
# 8081 — reserved for the admin listener (Plan 005 / /healthz)
EXPOSE 1701 8081

# if interval / retries / start-period change, update --wait-timeout in
# .github/workflows/release.yml (currently sized at 120s for the existing shape)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/httpproxy", "-healthcheck"]

ENTRYPOINT ["/httpproxy", "-config", "/etc/httpproxy/proxy.json"]
