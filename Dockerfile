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

# Buildx provides TARGETARCH per-platform leg (amd64 | arm64). We re-declare it
# in this stage so the codebase-memory-mcp fetch below picks the matching asset.
ARG TARGETARCH=amd64

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

# ─── codebase-memory-mcp (DeusData, MIT + Apache-2.0 nomic embeddings) ───────
# Code-graph + semantic codebase-memory MCP server, baked in so the
# @zibby/skills `codebase-memory` skill can spawn /usr/local/bin/codebase-memory-mcp
# with ZERO runtime download. We fetch the PORTABLE static build (works on this
# Debian/glibc base regardless of host libc), pin the version + sha256, and
# verify the checksum BEFORE extracting — never `curl | bash`. ARCH MISMATCH is
# the #1 trap: TARGETARCH (amd64|arm64) selects the matching release asset so
# the manifest list is correct on both Fargate x86 and ARM tasks.
#
# Attribution obligation (MIT + Apache-2.0): the LICENSE and
# THIRD_PARTY_NOTICES.md shipped in the tarball are copied to
# /usr/local/share/codebase-memory-mcp/NOTICES/.
#
# Bump CBM_VERSION + the matching sha256 together when upgrading. Checksums are
# the official release `checksums.txt` values for v0.8.1 (verified out-of-band).
ARG CBM_VERSION=0.8.1
ARG CBM_SHA256_AMD64=6ab87a6c05d049dde57700803ca0ab4199fcf25973a0606618af0fcee73f5abd
ARG CBM_SHA256_ARM64=13526acc2a6a0697dff3c763fb443a416589bc10ad8b12015b63d87e515dd72b
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) CBM_SHA="${CBM_SHA256_AMD64}" ;; \
      arm64) CBM_SHA="${CBM_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH} for codebase-memory-mcp" >&2; exit 1 ;; \
    esac; \
    asset="codebase-memory-mcp-linux-${TARGETARCH}-portable.tar.gz"; \
    url="https://github.com/DeusData/codebase-memory-mcp/releases/download/v${CBM_VERSION}/${asset}"; \
    tmp="$(mktemp -d)"; \
    curl -fsSL --retry 3 -o "${tmp}/${asset}" "${url}"; \
    echo "${CBM_SHA}  ${tmp}/${asset}" | sha256sum -c -; \
    tar -xzf "${tmp}/${asset}" -C "${tmp}"; \
    bin="$(find "${tmp}" -type f -name codebase-memory-mcp | head -n1)"; \
    test -n "${bin}"; \
    install -m 0755 "${bin}" /usr/local/bin/codebase-memory-mcp; \
    mkdir -p /usr/local/share/codebase-memory-mcp/NOTICES; \
    for f in LICENSE THIRD_PARTY_NOTICES.md; do \
      src="$(find "${tmp}" -type f -name "${f}" | head -n1)"; \
      if [ -n "${src}" ]; then cp "${src}" "/usr/local/share/codebase-memory-mcp/NOTICES/${f}"; fi; \
    done; \
    /usr/local/bin/codebase-memory-mcp --help >/dev/null 2>&1 || true; \
    rm -rf "${tmp}"

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
