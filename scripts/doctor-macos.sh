#!/usr/bin/env bash
# 9Router macOS doctor — detect and auto-fix the environment problems that make
# `bun run prod:local` fall back to Node-only (port NODE_PORT) with no Go
# front-door, or fail to build at all. All of these come from ONE thing:
# a repo/node_modules/bin copied from a Linux box instead of installed on macOS.
#
# Detects:
#   1. Stale checkout      — prod:local is the old Node-only script (no Go stack)
#   2. Cross-platform deps — node_modules built for Linux (swc .node not Mach-O)
#   3. Linux Go binary     — bin/9router-backend is ELF, can't exec on macOS
#   4. Missing Go toolchain — `go build` (build:backend) will fail
#   5. Incomplete .env     — JWT_SECRET / INITIAL_PASSWORD unset
#
# Usage:
#   scripts/doctor-macos.sh            # diagnose only (safe, read-only)
#   scripts/doctor-macos.sh --fix      # apply the safe fixes (reinstall, gen .env)
#   scripts/doctor-macos.sh --fix --reset-password
#                                      # ALSO wipe ~/.9router/db (destroys dashboard state)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

FIX=0; RESET_PW=0
for a in "$@"; do
  case "$a" in
    --fix) FIX=1 ;;
    --reset-password) RESET_PW=1 ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done

c_red=$'\033[31m'; c_grn=$'\033[32m'; c_yel=$'\033[33m'; c_cyn=$'\033[36m'; c_off=$'\033[0m'
ok()   { printf '%s  ok %s  %s\n'   "$c_grn" "$c_off" "$*"; }
warn() { printf '%s warn%s  %s\n'   "$c_yel" "$c_off" "$*"; }
bad()  { printf '%s FAIL%s  %s\n'   "$c_red" "$c_off" "$*"; }
act()  { printf '%s  -> %s  %s\n'   "$c_cyn" "$c_off" "$*"; }

ISSUES=0
note_issue() { ISSUES=$((ISSUES+1)); }

# is_macho FILE -> 0 if FILE is a Mach-O binary (native to macOS)
is_macho() { [ -f "$1" ] && file -b "$1" 2>/dev/null | grep -qi 'mach-o'; }

# --- platform guard ----------------------------------------------------------
if [ "$(uname -s)" != "Darwin" ]; then
  warn "not macOS ($(uname -s)) — this doctor targets macOS; nothing to do."
  exit 0
fi
ARCH="$(uname -m)"   # arm64 | x86_64
act "macOS $ARCH — running from $ROOT"
echo

# --- 1. stale checkout: prod:local must be the Go-stack script ----------------
if grep -q '"prod:local":[[:space:]]*"bash scripts/prod-local.sh"' package.json 2>/dev/null; then
  ok "prod:local uses the Go front-door script"
else
  bad "prod:local is the OLD Node-only script (no Go front-door → always NODE_PORT)"
  note_issue
  act "your checkout predates the Go stack. Update the 'go' branch:"
  echo "       git fetch origin && git checkout go && git pull origin go"
  # Can't safely auto-`git pull` (may have local changes / different remote) — tell, don't do.
fi

# --- 2. node_modules built for the wrong platform ----------------------------
SWC_PKG="@next/swc-darwin-${ARCH}"
[ "$ARCH" = "x86_64" ] && SWC_PKG="@next/swc-darwin-x64"
SWC_NODE="node_modules/${SWC_PKG}/next-swc.darwin-${ARCH}.node"
[ "$ARCH" = "x86_64" ] && SWC_NODE="node_modules/${SWC_PKG}/next-swc.darwin-x64.node"

NEED_REINSTALL=0
if [ ! -d node_modules ]; then
  warn "node_modules missing"
  NEED_REINSTALL=1; note_issue
elif ls node_modules/@next/swc-linux-* >/dev/null 2>&1 && ! is_macho "$SWC_NODE"; then
  bad "node_modules carries Linux swc and no valid macOS swc — copied from a Linux box"
  NEED_REINSTALL=1; note_issue
elif [ -e "$SWC_NODE" ] && ! is_macho "$SWC_NODE"; then
  bad "$SWC_PKG is present but not a valid Mach-O (truncated/wrong-arch — the __TEXT dlopen error)"
  NEED_REINSTALL=1; note_issue
elif [ ! -e "$SWC_NODE" ]; then
  warn "$SWC_PKG not installed (Next will fall back to slow WASM)"
  NEED_REINSTALL=1; note_issue
else
  ok "Next.js swc native binary is valid for $ARCH"
fi

if [ "$NEED_REINSTALL" = 1 ] && [ "$FIX" = 1 ]; then
  act "removing cross-platform artifacts and reinstalling with bun"
  rm -rf node_modules bun.lock
  if bun install; then ok "bun install completed"; else bad "bun install failed — see output above"; fi
fi

# --- 3. Go binary built for Linux --------------------------------------------
if [ -e bin/9router-backend ]; then
  if is_macho bin/9router-backend; then
    ok "bin/9router-backend is a native macOS binary"
  else
    bad "bin/9router-backend is not Mach-O (Linux ELF — won't exec; Go front-door dies at startup)"
    note_issue
    [ "$FIX" = 1 ] && { act "removing stale bin/ (prod-local will rebuild it)"; rm -rf bin; }
  fi
else
  warn "bin/9router-backend absent (prod-local will build it — needs Go)"
fi

# --- 4. Go toolchain ---------------------------------------------------------
if command -v go >/dev/null 2>&1; then
  ok "Go toolchain: $(go version | awk '{print $3}')"
else
  bad "Go toolchain not found — build:backend (go build) will fail"
  note_issue
  act "install it:  brew install go"
fi

# --- 5. .env completeness ----------------------------------------------------
gen_secret() { LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 48; }
env_has() { [ -f .env ] && grep -qE "^$1=.+" .env && ! grep -qE "^$1=(change-me|change-me-to-a-long-random-secret)$" .env; }

if [ ! -f .env ]; then
  bad ".env missing"
  note_issue
  if [ "$FIX" = 1 ]; then
    cp .env.example .env
    sec="$(gen_secret)"
    # BSD sed (macOS) needs the empty-string arg after -i
    sed -i '' "s|^JWT_SECRET=.*|JWT_SECRET=${sec}|" .env 2>/dev/null || sed -i "s|^JWT_SECRET=.*|JWT_SECRET=${sec}|" .env
    act "created .env from .env.example with a random JWT_SECRET"
    warn "INITIAL_PASSWORD is still 'change-me' — edit .env to set your own"
  fi
else
  if env_has JWT_SECRET; then ok "JWT_SECRET is set"; else
    bad "JWT_SECRET unset or still the placeholder — dashboard cookies won't validate across Go/Node"
    note_issue
    if [ "$FIX" = 1 ]; then
      sec="$(gen_secret)"
      if grep -qE '^JWT_SECRET=' .env; then
        sed -i '' "s|^JWT_SECRET=.*|JWT_SECRET=${sec}|" .env 2>/dev/null || sed -i "s|^JWT_SECRET=.*|JWT_SECRET=${sec}|" .env
      else printf '\nJWT_SECRET=%s\n' "$sec" >>.env; fi
      act "set a random JWT_SECRET in .env"
    fi
  fi
  if env_has INITIAL_PASSWORD; then ok "INITIAL_PASSWORD is customized"; else
    warn "INITIAL_PASSWORD is unset/placeholder — default login is 123456 (change it in .env)"
  fi
fi

# --- optional: password / DB reset (destructive) -----------------------------
DATA_DIR_ENV="${DATA_DIR:-}"
[ -z "$DATA_DIR_ENV" ] && [ -f .env ] && DATA_DIR_ENV="$(grep -E '^DATA_DIR=' .env | cut -d= -f2- | tr -d '"')"
DB_DIR="${DATA_DIR_ENV:-$HOME/.9router}/db"
if [ "$RESET_PW" = 1 ]; then
  if [ "$FIX" != 1 ]; then bad "--reset-password requires --fix"; exit 2; fi
  if [ -d "$DB_DIR" ]; then
    act "wiping $DB_DIR — this DELETES all dashboard state (accounts, providers, saved password)"
    rm -rf "$DB_DIR"
    ok "DB reset — next login uses INITIAL_PASSWORD (default 123456)"
  else
    ok "no DB at $DB_DIR — nothing to reset (login already uses INITIAL_PASSWORD)"
  fi
elif [ -d "$DB_DIR" ]; then
  warn "existing DB at $DB_DIR — if 123456 is rejected, a password was already set."
  echo "       reset with:  scripts/doctor-macos.sh --fix --reset-password"
fi

echo
if [ "$ISSUES" = 0 ]; then
  ok "no blocking issues. Start with:"
  echo "       WITH_HEADROOM=1 GO_PORT=21128 NODE_PORT=21129 bun run prod:local"
  echo "       then open  http://127.0.0.1:21128   (the Go front-door — NOT 21129)"
elif [ "$FIX" = 1 ]; then
  act "$ISSUES issue(s) addressed where possible. Re-run without --fix to confirm, then:"
  echo "       WITH_HEADROOM=1 GO_PORT=21128 NODE_PORT=21129 bun run prod:local"
else
  bad "$ISSUES issue(s) found. Re-run with --fix to apply the safe repairs:"
  echo "       scripts/doctor-macos.sh --fix"
fi
