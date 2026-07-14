#!/bin/sh
# 9Router container entrypoint — supervises the two-process reverse-proxy bridge.
#
#   Node engine (open-sse)  :20129   started in the background
#   Go backend              :20128   foreground (public entry point)
#
# The Go backend reverse-proxies every non-/health request to the Node engine.
# When Go exits, the Node engine is stopped too so the container can exit cleanly.
#
# Why no process manager (supervisord/s6)? Both binaries already handle their own
# graceful shutdown (SIGINT/SIGTERM), and a single trap keeps the image lean.

set -e

# Fix ownership of bind-mounted volumes (mounted volumes may be owned by root).
chown -R node:node /app/data /app/data-home 2>/dev/null || true

# --- 1) Start the Node engine (open-sse + Next.js standalone) -------------
# custom-server.js wraps the Next.js standalone server and reads PORT/HOSTNAME
# from the environment. We override PORT so it listens on the internal upstream
# port, not the public one (Go owns the public port).
echo "[entrypoint] starting Node engine on :${NINEROUTER_NODE_PORT:-20129}"
su-exec node env PORT="${NINEROUTER_NODE_PORT:-20129}" \
  HOSTNAME="${HOSTNAME:-127.0.0.1}" \
  node custom-server.js &
NODE_PID=$!

# --- Cleanup: stop the Node engine when Go exits -------------------------
cleanup() {
  trap - EXIT INT TERM
  if [ -n "${GO_PID:-}" ] && kill -0 "$GO_PID" 2>/dev/null; then
    echo "[entrypoint] stopping Go backend (pid $GO_PID)"
    kill -TERM "$GO_PID" 2>/dev/null || true
    wait "$GO_PID" 2>/dev/null || true
  fi
  if kill -0 "$NODE_PID" 2>/dev/null; then
    echo "[entrypoint] stopping Node engine (pid $NODE_PID)"
    kill -TERM "$NODE_PID" 2>/dev/null || true
    wait "$NODE_PID" 2>/dev/null || true
  fi
}
shutdown() {
  cleanup
  exit 0
}
trap cleanup EXIT
trap shutdown INT TERM

# --- 2) Wait for Node to be ready before starting Go ---------------------
# The Go reverse proxy returns 502 if the upstream is not yet up. Wait briefly
# so the very first client request does not race the Node startup. Use a plain
# TCP probe via node (already present in the image) to avoid extra deps.
NODE_PORT="${NINEROUTER_NODE_PORT:-20129}"
echo "[entrypoint] waiting for Node engine on 127.0.0.1:${NODE_PORT}"
i=0
while [ "$i" -lt 50 ]; do
  if su-exec node node -e '
    var s = require("net").connect('"${NODE_PORT}"', "127.0.0.1");
    s.on("connect", function () { s.end(); process.exit(0); });
    s.on("error", function () { process.exit(1); });
  ' 2>/dev/null; then
    echo "[entrypoint] Node engine is ready"
    break
  fi
  i=$((i + 1))
  sleep 0.2
done

# --- 3) Start the Go backend and supervise both processes -----------------
# Keep this shell as PID 1 so its trap can gracefully stop BOTH children.
# The Go backend binds loopback by default; containers explicitly expose it.
echo "[entrypoint] starting Go backend on :${PORT:-20128} (proxy -> :${NODE_PORT})"
su-exec node env GO_HOST="${GO_HOST:-0.0.0.0}" /usr/local/bin/9router-backend &
GO_PID=$!
GO_STATUS=0
wait "$GO_PID" || GO_STATUS=$?
exit "$GO_STATUS"
