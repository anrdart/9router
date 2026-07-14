# syntax=docker/dockerfile:1.7
#
# 9Router production image — two-process reverse-proxy bridge:
#   Go   :20128  public entry point (native /health, proxies everything else)
#   Node :20129  open-sse engine + Next.js standalone (the upstream)
#
# Multi-platform (linux/amd64 + linux/arm64) via buildx. The Go builder stage
# runs natively on each target platform so the binary always matches — no manual
# cross-compile needed.

ARG NODE_IMAGE=node:22-alpine
ARG GO_IMAGE=golang:1.24-alpine
ARG BUN_IMAGE=oven/bun:1-alpine

# ---------------------------------------------------------------------------
# Stage 1: build the Go backend binary.
# ---------------------------------------------------------------------------
FROM ${GO_IMAGE} AS go-builder
WORKDIR /src
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
# CGO disabled → static binary, runs on any base image. -s -w strip debug info.
# GOTOOLCHAIN=local prevents go from downloading a newer toolchain than the
# golang:1.24-alpine image ships (go.mod requests 1.24.x).
ENV CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=local
RUN go build -trimpath -ldflags="-s -w" -o /out/9router-backend ./cmd/server

# ---------------------------------------------------------------------------
# Stage 2: build the Next.js standalone bundle with Bun.
# ---------------------------------------------------------------------------
FROM ${BUN_IMAGE} AS builder
WORKDIR /app
# python3/make/g++ are needed to compile the optional better-sqlite3 native
# module; Bun builds it because it is in trustedDependencies.
RUN apk --no-cache upgrade && apk --no-cache add python3 make g++ linux-headers

# Copy the lockfile too so the build is reproducible (frozen lockfile).
COPY package.json bun.lock ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
  bun install --frozen-lockfile

COPY . ./
ENV NEXT_TELEMETRY_DISABLED=1
RUN bun run build

# ---------------------------------------------------------------------------
# Stage 3: runtime image with both binaries.
# ---------------------------------------------------------------------------
FROM ${NODE_IMAGE} AS runner
WORKDIR /app

LABEL org.opencontainers.image.title="9router"

ENV NODE_ENV=production
# Public port served by Go (unchanged from the historical entry point).
ENV PORT=20128
ENV HOSTNAME=0.0.0.0
ENV NEXT_TELEMETRY_DISABLED=1
ENV DATA_DIR=/app/data
# Node engine listens on the internal upstream port; Go proxies to it.
ENV NINEROUTER_NODE_PORT=20129
ENV NODE_UPSTREAM=http://127.0.0.1:20129

# --- Next.js standalone (Node engine / dashboard) -------------------------
COPY --from=builder /app/public ./public
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/custom-server.js ./custom-server.js
COPY --from=builder /app/open-sse ./open-sse
# Next file tracing can omit sibling files; MITM runs server.js as a separate process.
COPY --from=builder /app/src/mitm ./src/mitm
# Standalone node_modules may omit deps only required by the MITM child process.
COPY --from=builder /app/node_modules/node-forge ./node_modules/node-forge
# Ensure `next` is available at runtime in case tracing did not include it.
COPY --from=builder /app/node_modules/next ./node_modules/next

# --- Go backend binary + entrypoint ---------------------------------------
COPY --from=go-builder /out/9router-backend /usr/local/bin/9router-backend
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN mkdir -p /app/data && chown -R node:node /app && \
  mkdir -p /app/data-home && chown node:node /app/data-home && \
  ln -sf /app/data-home /root/.9router 2>/dev/null || true

RUN apk --no-cache upgrade && apk --no-cache add su-exec && \
  chmod +x /usr/local/bin/docker-entrypoint.sh

EXPOSE 20128

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
