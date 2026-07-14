# 9Router Migration Plan — Next.js → Go Backend

> Branch: `go` · Strategy: **Reverse-proxy bridge**
> Full audit: see [`MIGRATION_AUDIT.md`](./MIGRATION_AUDIT.md).

## Goal
Move core backend logic from Next.js route handlers into a Go backend, keeping
Next.js as the dashboard/frontend only. Go becomes the single public entry point.

## Why a reverse-proxy bridge (not a full rewrite)
The `open-sse/` engine is ~37,000 lines of JS with 95 provider adapters and 23
format translators. Porting it 1:1 is infeasible in one pass. Instead Go acts as
the front door, implementing endpoints natively one at a time, and transparently
reverse-proxying everything else to the Node engine — so **no feature is lost at
any point** and external clients (Cursor, Claude Code, Codex, Cline, Copilot,
OpenAI SDK) keep working unchanged.

## Architecture

```
Clients ──► :20128 Go backend ──┬── native: GET /health   (Phase 1)
                                ├── native: /api/* CRUD    (Phase 2+)
                                └── reverse-proxy ──► :20129 Node (open-sse engine)
Next.js dashboard (:20127) ──► fetch ──► :20128 Go
```

## Phases

### Phase 1 — DONE ✅ (this branch)
Go skeleton: `/health` native, everything else reverse-proxied to Node on 20129.
Graceful shutdown, structured logging (no secret leakage), CORS, body limits,
streaming passthrough, client-disconnect propagation. 18 unit/integration tests pass.

| File | Purpose |
|---|---|
| `backend/cmd/server/main.go` | entry point (config → server → graceful run) |
| `backend/internal/config/config.go` | env loader (PORT, NODE_UPSTREAM, …) |
| `backend/internal/server/server.go` | mux + middleware wiring + lifecycle |
| `backend/internal/middleware/*.go` | recovery, request-id, logging, CORS, body-limit |
| `backend/internal/proxy/reverse.go` | `httput.ReverseProxy` to Node upstream |
| `backend/internal/httpapi/health.go` | native `GET /health` |
| `backend/internal/**/*_test.go` | config, health, proxy (stream/disconnect/502), logging redaction |

### Phase 2 — DONE ✅ (this branch)
- `internal/database/` using `modernc.org/sqlite` v1.40.1 (pure Go, no CGO) —
  portable single-binary builds.
- Mirrors all 9 tables + indexes from `src/lib/db/schema.js` idempotently
  (`CREATE TABLE IF NOT EXISTS` + `ADD COLUMN` for missing columns), same PRAGMAs
  (WAL, busy_timeout=5000, …). Reads/writes the **same** DB file Node uses.
- Repos for settings, API keys (incl. constant-time `ValidateApiKey`), provider
  connections, and usage history.
- Native GET handlers: `/api/settings`, `/api/keys`, `/api/providers`,
  `/api/usage/logs`, `/api/usage/stats` — with secret redaction (password,
  oidcClientSecret stripped) and default-merging matching the Node handler.
- Minimal auth parity: `IsLocalRequest` + `RequireDashboardAuth` (local OK, else
  valid API key required; otherwise 401). Non-local requests without a key fall
  through to the Node proxy for full JWT/OIDC auth (Phase 3 will port that).
- Route rule: native handlers register an exact `METHOD /path`; any other method
  on the same path (e.g. `PATCH /api/settings`) falls through to the reverse
  proxy automatically — so mutations keep working via Node until ported.
- Verified against a **real 1.2 GB DB** (98,745 usage rows, 14 providers) and a
  full Docker container rebuild.

| File | Purpose |
|---|---|
| `backend/internal/database/database.go` | SQLite open (WAL, busy_timeout), `WithWriteLock`, path parity |
| `backend/internal/database/schema.go` | declarative schema + idempotent `SyncSchema` |
| `backend/internal/database/repos.go` | settings / apiKeys / providers / usage repos |
| `backend/internal/middleware/auth.go` | `IsLocalRequest`, `ExtractAPIKey`, `RequireDashboardAuth`, `WithDB` |
| `backend/internal/httpapi/settings.go` | `GET /api/settings` (secret redaction + defaults) |
| `backend/internal/httpapi/readonly.go` | `GET /api/keys`, `/api/providers`, `/api/usage/{logs,stats}` |

### Phase 3 — DONE ✅ (this branch)
Native read routes are now safe enough to stay in Go:
- Dashboard auth parity for native `/api/*` reads: shared `auth_token` JWT
  verification (stdlib HS256, same `DATA_DIR/jwt-secret` as Node), CLI machine
  token parity (`DATA_DIR/machine-id` + `auth/cli-secret`), `requireLogin=false`,
  and force-password-change lockout. Host-locality and LLM API keys no longer
  grant dashboard access.
- Go binds `127.0.0.1` by default; Docker explicitly sets `GO_HOST=0.0.0.0` so
  published container ports still work with auth enforced.
- `/api/providers` strips `apiKey`, `accessToken`, `refreshToken`, `idToken` and
  returns the Node-compatible `{ connections }` envelope.
- `/api/keys` returns the Node-compatible `{ keys }` envelope.
- `/api/usage/logs` returns Node-compatible formatted log strings and never emits
  raw API keys.
- `/api/usage/stats` ports the DB-backed aggregation with masked API keys. Live
  fields (`activeRequests`, `pending`, `errorProvider`) remain owned by Node's
  proxied `/api/usage/stream` SSE endpoint until the chat pipeline moves to Go.

### Phase 4 — Native OpenAI-compatible `/v1`
- `GET /v1/models` — **DONE ✅** (this branch). Native aggregation of DB combos +
  custom models + model aliases − disabled, over an embedded static catalog
  snapshot. Ports `buildModelsList(["llm"])` from `src/app/api/v1/models/route.js`.
  Transparently reverse-proxies the whole GET to Node **only** when an active
  connection needs live/network model resolution (kiro/qoder/kimchi/github/
  clinepass resolvers, or an OpenAI/Anthropic-compatible node with no explicit
  `enabledModels` and no static catalog) — so parity is exact in every case.
  Public route (the Node handler has no auth gate); the response exposes only
  model ids, never connection data or secrets.

  | File | Purpose |
  |---|---|
  | `scripts/gen-models-snapshot.mjs` | generator → `models_snapshot.json` (`bun run gen:models-snapshot`) |
  | `backend/internal/httpapi/models_snapshot.json` | **generated, committed** static catalog (`go:embed`) — do not hand-edit |
  | `backend/internal/httpapi/models.go` | native `GET /v1/models` + Node-proxy fallback |
  | `backend/internal/database/repos.go` | `ListCombos`, `KVScope` (customModels/modelAliases/disabledModels) |
  | `scripts/verify-models-parity.mjs` | diff native Go vs Node id sets (`bun run verify:models-parity`) |

  > **Snapshot upkeep:** `models_snapshot.json` is a generated artifact (like
  > `open-sse/providers/registry/index.js`). Regenerate with
  > `bun run gen:models-snapshot` after changing the provider registry or
  > `open-sse/config/providerModels.js`. The generator asserts its reconstructed
  > alias map against the live `PROVIDER_ID_TO_ALIAS` and fails loudly on drift.
  > The Go build stays Node-free (single-binary), so regeneration is manual.

- `POST /v1/chat/completions` — **NATIVE STRICT SLICE ✅**. JSON + SSE for
  catalogued LLMs on generated, simple OpenAI-compatible transports. Eligibility
  fails closed: explicit `stream`, plain OpenAI string messages, exactly one API-key
  account, no OAuth refresh/proxy/relay/token-saver/provider-thinking/model transform.
  Every unsupported body/provider/account replays its original bytes to Node.
- Native response normalization matches the existing OpenAI same-format path:
  required fields, Azure-field stripping, tool finish reason, usage buffer, SSE
  termination, invalid-id repair, estimated usage when upstream omits it.

### Phase 5 — Fallback routing + usage logging in Go
- **DONE for the native strict slice ✅**: OpenAI error envelopes, current-level
  exponential backoff, per-model locks/clear-on-success, moderation/client-error
  no-lock rules, transactional history+daily+lifetime writes, pricing overrides,
  sanitized/config-bounded request details.
- Multi-account selection/fallback remains on Node by design; the native gate proxies
  whenever more than one active account exists.

### Phase 6 — Cleanup
Remove/duplicate Next.js route handlers once Go is validated; multi-stage Docker
build (Go binary serving Next.js static + proxying dynamic to Node).

## Run instructions

### Prerequisites
- Bun 1.3+ (`bun --version`) — package manager and JS runtime for the Next.js dashboard/engine.
- Go 1.24+ (`go version`). If absent, install or run from the local SDK.

### Local development (three processes)
```bash
bun run dev:all
```
Starts (via `scripts/dev-all.js`, no extra dependency):
- **go**  `:20128` — public entry point (`bun run dev:backend`)
- **node** `:20129` — open-sse engine, the proxy upstream
- **ui**  `:20127` — Next.js dashboard

Point clients at `http://localhost:20128/v1` (unchanged).

### Individual commands
| What | Command | Port |
|---|---|---|
| Go backend only | `bun run dev:backend` | 20128 |
| Node engine (upstream) | `bun run dev:node-upstream` | 20129 |
| Next.js dashboard | `bun run dev` | 20127 |
| Build Go binary | `bun run build:backend` → `bin/9router-backend` | — |
| Go tests | `bun run test:backend` | — |

### Environment (`.env`)
See `.env.example`. Key additions for the bridge:
```
NODE_UPSTREAM=http://127.0.0.1:20129
NINEROUTER_NODE_PORT=20129
# Optional direct-run exposure; default is loopback-only:
# GO_HOST=0.0.0.0
```

## Security (current baseline)
- Native `/api/*` reads require dashboard JWT, CLI machine token, or `requireLogin=false`.
  Host-locality and inbound LLM API keys do **not** grant dashboard access.
- Go binds `127.0.0.1` by default; Docker sets `GO_HOST=0.0.0.0` explicitly.
- Native provider/usage responses strip/mask stored provider credentials and API keys.
- Go logs **never** include `Authorization`, `X-Api-Key`, `X-9R-*`, `Cookie`
  (verified by `TestLoggingNoSecretsInLine`).
- Request bodies capped at 128 MiB (configurable via `GO_BODY_MAX_MB`).
- Read/write/idle timeouts; graceful drain on SIGINT/SIGTERM.

## Verification (Definition of Done — Phase 1)
- [x] `go build ./cmd/server` and `go test ./...` pass.
- [x] Go starts on 20128 without error.
- [x] `GET /health` → 200 `{ok:true}`.
- [x] `GET /v1/models` via Go is transparently proxied to Node.
- [x] Streaming `/v1/chat/completions` passes through chunk-by-chunk (no buffering).
- [x] Client disconnect during streaming cancels the upstream request (no leak).
- [x] Authorization header absent from Go logs.

## Verification (Definition of Done — Phase 2)
- [x] `go test ./...` passes (DB open, schema sync, settings roundtrip, validateApiKey,
      providers merge, usage order, auth gating, secret redaction — ~30 tests).
- [x] Go opens the **same** SQLite DB Node uses (1.2 GB real file) without error.
- [x] `GET /api/settings` native → matches Node shape, secrets stripped.
- [x] `GET /api/providers` native → 14 real connections with merged data blobs.
- [x] `GET /api/usage/stats` native → 98,745 rows.
- [x] Non-local request without API key → 401; `PATCH /api/settings` → proxied to Node.
- [x] Docker container (two-process bridge) rebuilds and runs with the real DB volume.
- [x] Graceful shutdown (`docker stop`) exits cleanly in <0.2 s.

## Verification (Definition of Done — Phase 3)
- [x] `go test ./...` covers auth JWT/CLI/force-change gates, settings/env flags,
      provider redaction/envelope, usage log no-key-leak, DB-only stats aggregation,
      and the previous single-connection deadlock trap.
- [x] Valid dashboard JWT cookie → native `/api/*` 200; expired/missing JWT → 401.
- [x] Force-password-change JWT → `/api/settings` allowed, `/api/keys` 403.
- [x] Valid LLM API key alone no longer grants dashboard/native `/api/*` access.
- [x] `GET /api/providers` strips `apiKey/accessToken/refreshToken/idToken` and wraps `{connections}`.
- [x] `GET /api/usage/logs` returns formatted strings and cannot emit raw API keys.
- [x] `GET /api/usage/stats` masks API keys and emits empty live fields for Node SSE overlay.
- [x] Default Go bind is `127.0.0.1`; Docker exposes by setting `GO_HOST=0.0.0.0`.
