# syntax=docker/dockerfile:1.7

# ─── build stage ───────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS build

# Build args wired by GoReleaser / CI for embedding version in the binary.
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# Dependency layer — caches as long as go.{mod,sum} don't change.
COPY go.mod go.sum ./
RUN go mod download

# Source layer.
COPY . .

# Static binary (modernc.org/sqlite is pure-Go so CGO can stay off).
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/agent-opsd ./cmd/agent-opsd

# ─── runtime stage ─────────────────────────────────────────────────────────
# alpine, not scratch — we want a real /bin/sh because the shell tool relies
# on it. Static binary + tiny base = ~12MB image.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata busybox-extras curl nodejs npm \
    && npm install -g --no-audit --no-fund @anthropic-ai/claude-code \
    && adduser -D -u 10000 agentops \
    && mkdir -p /var/lib/agent-ops /etc/agent-ops \
    && chown -R agentops:agentops /var/lib/agent-ops /etc/agent-ops

COPY --from=build /out/agent-opsd /usr/local/bin/agent-opsd

# Bake the documented example config as the default so the image runs
# without a mounted volume. Per-instance prompts come via the
# AGENT_OPS_BOOTSTRAP_PROMPT env var (see internal/config/config.go).
# Mount your own /etc/agent-ops/config.yaml at runtime to fully override.
COPY config.example.yaml /etc/agent-ops/config.yaml

USER agentops
WORKDIR /home/agentops

EXPOSE 7842

HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:7842/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/agent-opsd"]
CMD ["--config", "/etc/agent-ops/config.yaml"]
