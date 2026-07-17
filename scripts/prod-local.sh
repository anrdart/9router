#!/usr/bin/env bash
# Run the 9Router production stack locally, WITHOUT Docker.
#
#   Node (open-sse engine)  ->  NODE_PORT (default 21129, loopback)
#   Go   (public front-door) ->  GO_PORT   (default 21128, loopback)
#
# The Go front-door reverse-proxies everything except its native /health and the
# authenticated /api reads to the Node engine. Both processes must share the same
# JWT_SECRET (from .env) or dashboard cookies minted by Node are rejected by Go —
# this script sources .env so they match, mirroring Docker's --env-file.
#
# Usage:
#   scripts/prod-local.sh [start|stop|status|restart]   (default: start)
#
# Env overrides:
#   GO_PORT=21128 NODE_PORT=21129 SKIP_BUILD=1 scripts/prod-local.sh
#   WITH_HEADROOM=1 HEADROOM_PORT=8787 scripts/prod-local.sh   # also run Token Saver proxy
#
# start   build (unless SKIP_BUILD=1) + launch Node+Go (+headroom if WITH_HEADROOM=1),
#         wait until healthy, print URL. Refuses ports owned by other processes.
# stop    stop only the processes THIS script started (tracked by pidfiles)
# status  show listeners + health of go/node (+headroom)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO_PORT="${GO_PORT:-21128}"
NODE_PORT="${NODE_PORT:-21129}"
RUN_DIR="${RUN_DIR:-/tmp/9router-prod}"
GO_PIDFILE="$RUN_DIR/go.pid"
NODE_PIDFILE="$RUN_DIR/node.pid"
GO_LOG="$RUN_DIR/go.log"
NODE_LOG="$RUN_DIR/node.log"
mkdir -p "$RUN_DIR"

# Headroom (Token Saver) is opt-in: WITH_HEADROOM=1. It runs as its own process
# (the `headroom` Python CLI) and is optional to the core Go+Node stack.
WITH_HEADROOM="${WITH_HEADROOM:-0}"
HEADROOM_PORT="${HEADROOM_PORT:-8787}"
# Reuse the app's own pidfile so the dashboard's managed status stays coherent
# with what this script starts. DATA_DIR is sourced from .env below (default
# ~/.9router, matching src/lib/dataDir.js).

log()  { printf '\033[36m[prod-local]\033[0m %s\n' "$*"; }
err()  { printf '\033[31m[prod-local]\033[0m %s\n' "$*" >&2; }

# port_busy PORT -> 0 if something is LISTENing on it.
# ss is Linux-only; fall back to lsof (macOS/BSD) so the guard actually works there.
port_busy() {
  if command -v ss >/dev/null 2>&1; then
    ss -ltn "sport = :$1" 2>/dev/null | grep -q LISTEN
  elif command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  else
    return 1   # no probe available → assume free (best effort)
  fi
}

# list_listeners PORT... -> print LISTEN sockets on the given ports (portable).
list_listeners() {
  if command -v ss >/dev/null 2>&1; then
    local expr=""
    for p in "$@"; do expr="${expr:+$expr or }sport = :$p"; done
    ss -ltn "$expr" 2>/dev/null
  elif command -v lsof >/dev/null 2>&1; then
    local args=() p
    for p in "$@"; do args+=(-iTCP:"$p"); done
    lsof -nP "${args[@]}" -sTCP:LISTEN 2>/dev/null
  fi
}

# pidfile_alive FILE -> echoes pid if the recorded process is still alive
pidfile_alive() {
  local f="$1" pid
  [ -f "$f" ] || return 1
  pid="$(cat "$f" 2>/dev/null || true)"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && { echo "$pid"; return 0; }
  return 1
}

wait_health() { # wait_health URL LABEL [PIDFILE] [LOGFILE]
  local url="$1" label="$2" pidfile="${3:-}" logfile="${4:-}" i
  for i in $(seq 1 60); do
    curl -sf -m2 -o /dev/null "$url" && { log "$label ready"; return 0; }
    # Fail fast if the process we're waiting on already died (e.g. a Linux
    # binary that can't exec on macOS) instead of burning the full 30s.
    if [ -n "$pidfile" ] && ! pidfile_alive "$pidfile" >/dev/null; then
      err "$label process exited before becoming healthy"
      [ -n "$logfile" ] && [ -f "$logfile" ] && { err "last lines of $logfile:"; tail -n 20 "$logfile" >&2; }
      return 1
    fi
    sleep 0.5
  done
  err "$label did not become healthy at $url"
  [ -n "$logfile" ] && [ -f "$logfile" ] && { err "last lines of $logfile:"; tail -n 20 "$logfile" >&2; }
  return 1
}

# --- Headroom (Token Saver) --------------------------------------------------
# Uses the app's own pidfile ($DATA_DIR/headroom/proxy.pid) and the same
# `headroom proxy --port N` invocation as src/lib/headroom/process.js.
headroom_dir()     { echo "${DATA_DIR:-$HOME/.9router}/headroom"; }
headroom_pidfile() { echo "$(headroom_dir)/proxy.pid"; }
headroom_bin()     { command -v headroom 2>/dev/null || echo "$HOME/.local/bin/headroom"; }

# A pid is a live headroom proxy only if its cmdline references headroom — mirrors
# the pid-reuse guard in getManagedPid() so a recycled pid doesn't fool us.
headroom_managed_pid() {
  local pf pid; pf="$(headroom_pidfile)"
  [ -f "$pf" ] || return 1
  pid="$(cat "$pf" 2>/dev/null || true)"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null || { rm -f "$pf"; return 1; }
  # Linux: /proc/<pid>/cmdline. macOS/BSD: no procfs — use `ps -o command`.
  if grep -qai headroom "/proc/$pid/cmdline" 2>/dev/null \
     || ps -p "$pid" -o command= 2>/dev/null | grep -qai headroom; then
    echo "$pid"; return 0
  fi
  rm -f "$pf"; return 1   # stale (pid reused by another process)
}

# Rewrite settings.headroomUrl to loopback in SQLite if it is currently a
# non-loopback host (e.g. the Docker "headroom" service name). No effect if the
# DB/row/key is absent; getSettings() then falls back to the localhost default.
ensure_headroom_url_loopback() {
  local db="${DATA_DIR:-$HOME/.9router}/db/data.sqlite"
  [ -f "$db" ] || return 0
  DB_PATH="$db" WANT_URL="http://localhost:$HEADROOM_PORT" python3 - <<'PY' || err "headroomUrl rewrite skipped"
import json, os, sqlite3, urllib.parse
db, want = os.environ["DB_PATH"], os.environ["WANT_URL"]
loop = {"localhost", "127.0.0.1", "::1", "[::1]", "0.0.0.0"}
c = sqlite3.connect(db)
row = c.execute("SELECT data FROM settings WHERE id=1").fetchone()
if not row: raise SystemExit(0)
d = json.loads(row[0] or "{}")
cur = d.get("headroomUrl")
host = None
if cur:
    try: host = urllib.parse.urlparse(cur).hostname
    except Exception: host = None
if host in loop: raise SystemExit(0)   # already loopback → nothing to do
d["headroomUrl"] = want
c.execute("UPDATE settings SET data=? WHERE id=1", (json.dumps(d),))
c.commit()
print(f"[prod-local] headroomUrl: {cur!r} -> {want}")
PY
}

start_headroom() {
  local bin pid; bin="$(headroom_bin)"
  [ -x "$bin" ] || { err "headroom CLI not found (~/.local/bin/headroom) — skipping"; return 0; }
  if pid="$(headroom_managed_pid)"; then log "headroom already running (pid $pid)"; return 0; fi
  mkdir -p "$(headroom_dir)"
  log "starting headroom proxy on 127.0.0.1:$HEADROOM_PORT"
  nohup "$bin" proxy --port "$HEADROOM_PORT" >"$(headroom_dir)/proxy.log" 2>&1 &
  echo $! >"$(headroom_pidfile)"
  wait_health "http://localhost:$HEADROOM_PORT/health" "headroom" || {
    err "headroom failed to start — see $(headroom_dir)/proxy.log"; return 1; }
}

stop_headroom() {
  local pid; if pid="$(headroom_managed_pid)"; then
    log "stopping headroom (pid $pid)"; kill "$pid" 2>/dev/null || true
  fi
  rm -f "$(headroom_pidfile)"
}

do_start() {
  # Refuse to trample a port that something ELSE already owns (not our pidfile).
  if port_busy "$NODE_PORT" && ! pidfile_alive "$NODE_PIDFILE" >/dev/null; then
    err "port $NODE_PORT is in use by another process — set NODE_PORT=<free port> and retry"; exit 1
  fi
  if port_busy "$GO_PORT" && ! pidfile_alive "$GO_PIDFILE" >/dev/null; then
    err "port $GO_PORT is in use by another process — set GO_PORT=<free port> and retry"; exit 1
  fi

  # Shared secrets: source .env so Go inherits JWT_SECRET/DATA_DIR like Node.
  if [ -f .env ]; then set -a; . ./.env; set +a; else err "no .env found — JWT auth will not match Node"; fi
  [ -n "${JWT_SECRET:-}" ] || err "JWT_SECRET unset — dashboard login via Go will 401"

  if [ "${SKIP_BUILD:-0}" != "1" ]; then
    # Rebuilding rewrites .next/ (new asset hashes). A next-start from a prior run
    # still holds the OLD build in memory → it serves stale hashes (500s) and the
    # "already running" guards below would keep it alive. Stop our old processes
    # first so the rebuild never lands under a live server.
    if pidfile_alive "$NODE_PIDFILE" >/dev/null || pidfile_alive "$GO_PIDFILE" >/dev/null; then
      log "stopping prior processes before rebuild (avoids stale-.next 500s)"
      do_stop
    fi
    log "building Go backend + Next.js (default output)…"
    bun run build:backend
    NEXT_OUTPUT=default NEXT_TELEMETRY_DISABLED=1 bun run build
  else
    log "SKIP_BUILD=1 → using existing bin/ and .next/"
  fi

  # 1) Node upstream (loopback).
  if pidfile_alive "$NODE_PIDFILE" >/dev/null; then
    log "node upstream already running (pid $(cat "$NODE_PIDFILE"))"
  else
    log "starting node upstream on 127.0.0.1:$NODE_PORT"
    NODE_ENV=production nohup bun --bun next start \
      --port "$NODE_PORT" -H 127.0.0.1 >"$NODE_LOG" 2>&1 &
    echo $! >"$NODE_PIDFILE"
  fi
  wait_health "http://127.0.0.1:$NODE_PORT/api/health" "node upstream" "$NODE_PIDFILE" "$NODE_LOG"

  # 2) Go front-door (loopback). GO_HOST default is 127.0.0.1 in the binary.
  if pidfile_alive "$GO_PIDFILE" >/dev/null; then
    log "go front-door already running (pid $(cat "$GO_PIDFILE"))"
  else
    log "starting go front-door on 127.0.0.1:$GO_PORT (upstream :$NODE_PORT)"
    GO_PORT="$GO_PORT" NODE_UPSTREAM="http://127.0.0.1:$NODE_PORT" \
      nohup ./bin/9router-backend >"$GO_LOG" 2>&1 &
    echo $! >"$GO_PIDFILE"
  fi
  if ! wait_health "http://127.0.0.1:$GO_PORT/health" "go front-door" "$GO_PIDFILE" "$GO_LOG"; then
    [ "$(uname -s)" = "Darwin" ] && err "on macOS this is usually a cross-platform checkout — run: bun run doctor:macos --fix"
    exit 1
  fi

  # 3) Headroom (opt-in). A DB value like http://headroom:8787 (a Docker service
  # name) is not loopback, so the dashboard marks it "External" and disables
  # Start/Stop. Rewrite it to loopback directly in SQLite (auth-free, and the
  # /api/settings PATCH would 401 without a session anyway), then launch.
  if [ "$WITH_HEADROOM" = "1" ]; then
    ensure_headroom_url_loopback
    start_headroom || true
  fi

  log "stack up →  http://127.0.0.1:$GO_PORT   (logs: $GO_LOG, $NODE_LOG)"
}

do_stop() {
  local pid
  # Stop headroom if THIS script's pidfile owns a live headroom proxy.
  [ -f .env ] && { set -a; . ./.env; set +a; }
  stop_headroom
  for pf in "$GO_PIDFILE" "$NODE_PIDFILE"; do
    if pid="$(pidfile_alive "$pf")"; then
      log "stopping pid $pid ($(basename "$pf"))"; kill "$pid" 2>/dev/null || true
    fi
    rm -f "$pf"
  done
  log "stopped (only processes started by this script)"
}

do_status() {
  log "listeners:"; list_listeners "$GO_PORT" "$NODE_PORT" | sed 's/^/  /' || true
  curl -s -m3 -o /dev/null -w "  go   /health          -> %{http_code}\n" "http://127.0.0.1:$GO_PORT/health"     || echo "  go   unreachable"
  curl -s -m3 -o /dev/null -w "  node /api/health      -> %{http_code}\n" "http://127.0.0.1:$NODE_PORT/api/health" || echo "  node unreachable"
  [ -f .env ] && { set -a; . ./.env; set +a; }
  curl -s -m3 -o /dev/null -w "  headroom /health      -> %{http_code}\n" "http://localhost:$HEADROOM_PORT/health" || echo "  headroom unreachable"
}

case "${1:-start}" in
  start)   do_start ;;
  stop)    do_stop ;;
  restart) do_stop; do_start ;;
  status)  do_status ;;
  *) err "usage: $0 [start|stop|status|restart]"; exit 2 ;;
esac
