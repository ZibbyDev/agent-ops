# syntax=docker/dockerfile:1.7

# ─── build stage ───────────────────────────────────────────────────────────
# Build still happens on Alpine because the daemon binary is CGO-disabled
# pure Go — libc doesn't matter here. Smaller cache layer, faster CI.
FROM golang:1.23-alpine AS build

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/agent-opsd ./cmd/agent-opsd \
 && GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/agent-ops ./cmd/agent-ops
# agent-ops (the CLI) ships alongside agent-opsd in the image so `docker exec
# <name> agent-ops status` / `agent-ops mcp token` / `agent-ops doctor` work
# inside the container without a separate install. The ENTRYPOINT still
# points at agent-opsd — agent-ops is opt-in.

# ─── runtime stage ─────────────────────────────────────────────────────────
# Switched off Alpine (musl) to Debian (glibc) so catalog scripts can install
# the entire `manylinux` Python/Node ecosystem — PyTorch / playwright /
# onnxruntime / sentence-transformers etc all ship glibc-only wheels with
# no Alpine equivalent. node:20-bookworm-slim already ships Node 20 + npm
# so we don't need a separate NodeSource setup step.
FROM node:20-bookworm-slim

# `apt-get install` defaults to interactive prompts; non-interactive lets
# `tzdata` and friends install silently.
ENV DEBIAN_FRONTEND=noninteractive

# Order matters here. ca-certificates is what makes HTTPS apt requests
# possible — without it, the first HTTPS fetch dies with "certificate
# verification failed." So we install ca-certificates using the default
# (http://) sources first (GitHub Actions build runner has direct
# internet, no proxy, so http works), THEN sed-rewrite the sources to
# https:// so every runtime apt-get call by catalog scripts goes through
# the Managed Apps egress proxy correctly.
#
# Why HTTPS at runtime: the egress proxy is a CONNECT-only tunnel; it
# rejects plain-HTTP GETs with 405. Switching apt's sources to https://
# makes apt do CONNECT deb.debian.org:443 first and tunnel the GET
# inside, which the proxy forwards normally.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        tzdata \
        curl \
        bash \
        procps \
 && rm -rf /var/lib/apt/lists/* \
 && sed -i 's|http://deb.debian.org|https://deb.debian.org|g; s|http://security.debian.org|https://security.debian.org|g' \
        /etc/apt/sources.list.d/debian.sources \
 && npm install -g --no-audit --no-fund @anthropic-ai/claude-code \
 && npm install -g --no-audit --no-fund @openai/codex@0.135.0 \
 && mkdir -p /var/lib/agent-ops /etc/agent-ops
# ^ @openai/codex (the OpenAI Codex CLI) backs the `codex` provider in
# agent-ops's buildDriver switch. Pinned to 0.135.0 — bump deliberately
# when we want to pick up new CLI flags or NDJSON event shapes (the
# driver's parser is forward-compatible, but new event types worth
# surfacing in slog need explicit support). Codex CLI declares
# `engines: node >= 16`, so the existing node:20-bookworm-slim base
# satisfies it; no base-image bump required. Auth at runtime via the
# OPENAI_API_KEY env var, which Codex reads natively — agent-ops does
# not pass it on the command line.

COPY --from=build /out/agent-opsd /usr/local/bin/agent-opsd
COPY --from=build /out/agent-ops /usr/local/bin/agent-ops

COPY config.example.yaml /etc/agent-ops/config.yaml

# Claude Code CLI's Bash tool requires /bin/bash explicitly; SHELL=/bin/bash
# is on Debian by default but we set it for parity with the old Alpine image.
ENV SHELL=/bin/bash

# Catalog install scripts need root for apt-get / mount / etc. The container
# is single-tenant (one Fargate task per managed-app instance) so the
# isolation boundary is the container, not the in-container UID.
USER root
WORKDIR /root

EXPOSE 7842

HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:7842/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/agent-opsd"]
CMD ["--config", "/etc/agent-ops/config.yaml"]
