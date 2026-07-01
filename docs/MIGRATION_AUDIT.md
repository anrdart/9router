# 9Router Migration Audit — Next.js → Go Backend

> Branch: `go` · Date: 2026-07-01
> Goal: Move core backend logic from Next.js route handlers into a Go backend on port 20128.
> Next.js becomes dashboard/frontend only.

---

## 1. Current Architecture

**Stack:** Next.js 16 (App Router) + custom-server.js (Express + http-proxy-middleware) + SQLite
(sql.js primary / better-sqlite3 optional) + zustand. Node 24.

**Engine:** `open-sse/` — a **36,933-line** JS engine that is the heart of the app:
- `providers/registry/` — **95 provider adapters** (openai, anthropic, gemini, kiro, cursor, codex,
  qoder, xai, deepseek, qwen, ollama, github, and ~80 more).
- `translator/request|response/` — **23 format translators** (openai↔claude↔gemini↔kiro↔cursor↔vertex↔ollama).
- `executors/` — **24 executors** incl. web-scraping ones (cursor, grok-web, perplexity-web, antigravity).
- `handlers/chatCore.js` — core chat routing/streaming/translation pipeline.
- `services/accountFallback.js` — config-driven fallback + exponential backoff + model-lock cooldown.
- `services/combo.js` — multi-model "combo"/fusion routing.

**Persistence:** SQLite via `src/lib/db/` (adapter pattern: betterSqlite / bunSqlite / nodeSqlite / sqljs).
9 tables: `settings, providerConnections, providerNodes, proxyPools, apiKeys, combos, kv, usageHistory,
usageDaily, requestDetails`. WAL mode.

**Auth:** JWT (jose) + bcryptjs password for dashboard; per-instance API keys (`apiKeys` table);
OAuth credential manager with token refresh for ~15 providers.

---

## 2. Endpoint Inventory

**131 Next.js route handlers** total. Grouped by priority for migration:

### A. OpenAI-compatible `/v1` — used by external tools (HIGH priority)
| Endpoint | Methods | Source |
|---|---|---|
| `/v1/chat/completions` | POST | `src/app/api/v1/chat/completions/route.js` → `src/sse/handlers/chat.js` → `open-sse/handlers/chatCore.js` |
| `/v1/models` | GET | `src/app/api/v1/models/route.js` |
| `/v1/models/[kind]`, `/v1/models/info` | GET | (model catalog) |
| `/v1/messages` (Anthropic-format) | POST | → `src/sse/handlers/chat.js` |
| `/v1/messages/count_tokens` | POST | |
| `/v1/responses`, `/v1/responses/compact` (OpenAI Responses API) | POST | |
| `/v1/embeddings` | POST | → `src/sse/handlers/embeddings.js` |
| `/v1/audio/speech|transcriptions|voices` | POST/GET | TTS/STT |
| `/v1/images/generations` | POST | image gen |
| `/v1/search` | POST | web search proxy |
| `/v1/web/fetch` | POST | reader proxy |
| `/v1/api/chat` | POST | (legacy alias) |
| `/v1beta/models`, `/v1beta/models/[...path]` | GET | Gemini-format compat |

### B. Internal `/api` — used by dashboard (MEDIUM priority, ~115 routes)
Categories (full list in §3):
- **settings** (1 route) — config, password, OIDC flags
- **providers** (~12) — CRUD connections, test, test-batch, models, validate
- **provider-nodes** (3), **proxy-pools** (~7), **combos** (2)
- **keys** (2) — instance API key CRUD
- **models** (~7) — aliases, availability, custom, disabled, test
- **usage** (~10) — history, logs, chart, stats, request-details, stream
- **auth** (~7) — login/logout/status/reset/oidc
- **cli-tools** (~18) — write config files for Claude/Codex/Cursor/Cline/Copilot etc.
- **oauth** (~16) — import/refresh credentials per provider
- **translator** (6), **tunnel** (7), **mcp** (2), **media-providers** (6), **headroom** (3), **health** (1), **init** (1)

### C. Frontend contract
Dashboard calls **109 unique** fetch paths — all relative `/api/*` and `/v1/*`.
No central base URL today (calls use relative paths, Next.js serves same origin).

---

## 3. Risk Assessment

| Risk | Severity | Notes |
|---|---|---|
| `open-sse/` engine is **37k lines** with 95 providers + web-scraping executors | **Critical** | Cannot be ported 1:1 to Go in reasonable time. **Must start with proxy mode**, port adapters incrementally. |
| Streaming SSE pipeline (stream.js 480 lines, sseToJsonConverter, responsesTransformer) | **High** | Format translation mid-stream is complex. Phase 4 focus. |
| Combo/fusion routing + model-lock backoff | **High** | Lots of state. Port after single-provider works. |
| OAuth token refresh for 15 providers | **Medium** | Provider-specific. Port per-provider as needed. |
| MITM server (cert generation, per-CLI intercept) | **Medium** | Separate process already. Can stay in Node longest. |
| DB adapter shims (sql.js fallback) | **Low** | Go uses modernc.org/sqlite (pure Go) — single adapter. |
| CLI tools write files to user's machine | **Low** | Stays in dashboard; not a backend concern. |

---

## 4. Key Files (Backend Logic to Eventually Move)

| Concern | File |
|---|---|
| Chat entry | `src/sse/handlers/chat.js` |
| Chat core pipeline | `open-sse/handlers/chatCore.js` |
| Provider credential selection + fallback | `src/sse/services/auth.js`, `open-sse/services/accountFallback.js` |
| Provider registry | `open-sse/providers/registry/*.js` (95 files) |
| Translators | `open-sse/translator/{request,response}/*.js` |
| Streaming utils | `open-sse/utils/stream.js`, `sse.js`, `streamHandler.js` |
| DB layer | `src/lib/db/index.js`, `schema.js`, `repos/*.js` |
| Settings | `src/lib/localDb.js` → `src/lib/db/repos/settingsRepo.js` |
| Error rules / backoff | `open-sse/config/errorConfig.js`, `runtimeConfig.js` |
