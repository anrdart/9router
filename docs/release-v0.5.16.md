# v0.5.16 — Security, correctness & reliability fixes

This release patches **23 bugs** across the proxy engine, format translators, persistence layer, and security boundary. It includes two **behavioral changes** that improve default security posture — please review them before upgrading exposed installations.

## ⚠️ Breaking / behavioral changes

### 1. API keys are now required by default for the LLM endpoints
`requireApiKey` now defaults to **`true`** (fail-closed). On a fresh or upgraded install where the LLM API is reachable beyond loopback, requests without a valid API key are rejected with `401`. Loopback clients remain exempt.

- **Why:** Previously the LLM API was unauthenticated by default, so any externally-exposed install (tunnel, Tailscale, LAN bind) let anyone consume your configured provider credentials (Claude Pro, Codex, Copilot, …).
- **If you relied on the old behavior:** create an API key in the dashboard, or explicitly set `requireApiKey: false` after weighing the risk.

### 2. Remote login with the default password no longer grants full access
Logging in remotely with the default password (`123456`) now issues a **short-lived token scoped to setting a new password only**, instead of a full-access dashboard session. A leaked/known default can no longer expose stored provider credentials.

- **Action:** set a password on first use.

---

## Security

- **Auth bypass (P0):** `requireApiKey` defaults to true — unauthenticated LLM API access on exposed installs is closed.
- **Header spoofing (P0):** `x-9r-real-ip` is only trusted when accompanied by `x-9r-via-proxy` (set by `custom-server`). Under bare `next start` a remote attacker can no longer spoof a loopback origin.
- **Default-password remote login (P0):** issues a force-password-change token (10m, password-set only) instead of full access.
- **MITM sudo-password encryption (P0):** AES key now derived from a persisted random secret + machineId (was recoverable from public machineId alone); backward-compatible decrypt for legacy blobs.
- **MITM Root CA key:** `rootCA.key` is now written `0600` (was world-readable); an existing key from an older version is also tightened on next run.
- **API key comparison:** constant-time (`timingSafeEqual`) instead of short-circuiting equality — defeats timing attacks that recover a key byte-by-byte.
- **Login limiter:** trust model aligned with the locality check; loopback bypasses lockout so a remote attacker can't DOS-lockout the admin.
- **CSRF:** Origin/Referer check on state-changing `/api/*` mutations (defense-in-depth beyond SameSite=Lax).

## Correctness

- **Kiro multi-tool calls:** monotonic tool-call index. The previous hardcoded `index: 0` corrupted tool arguments and silently dropped every tool call after the first in a multi-tool turn.
- **Account fallback:** HTTP 400/422 now fail fast instead of retrying the identical request against every account and locking the whole pool for 30s.
- **Executor concurrency:** per-request `AsyncLocalStorage` context for codex/gemini-cli/antigravity/opencode-go. Concurrent requests to the same provider no longer cross-contaminate session id, compact flag, or model.
- **Token refresh dedup:** `null` results are no longer cached (was taking connections offline for 10s on transient failures); dedup key is scoped per connection to avoid cross-connection token-rotation desync.

## Reliability / persistence

- **sql.js:** atomic write (temp + rename) and robust shutdown flush — no DB corruption or lost writes on crash/power-loss/container stop.
- **Backup/restore:** `exportDb`/`importDb` now round-trip usage history, request details, disabled models, and the lifetime counter (previously dropped); per-row error isolation so one bad row no longer aborts the whole import; backups include WAL sidecar files for a complete snapshot.
- **Migrations:** `ADD COLUMN ... NOT NULL` now supplies a matching `DEFAULT` (was silently failing and breaking the table).
- **Pending-request accounting:** ref-counted safety timer — concurrent requests no longer disarm each other's timer (which left phantom stuck requests).
- **401/403 retry:** the original error response body is cancelled (was leaking an upstream socket per refresh).
- **Combo rotation:** serialized per-combo so a rotation step isn't lost under concurrent load; `Retry-After` is now surfaced to the client.

## Translators

- **Responses API:** unique monotonic `output_index` per item (reasoning/text/tool no longer collide on index 0).
- **Reasoning:** multiple `<think>` blocks in one turn each open their own reasoning item.
- **Tool responses:** `fixMissingToolResponses` scans ahead — no more spurious empty tool results that 400'd on strict providers.
- **RTK:** error traces are preserved for OpenAI tool output (matching the existing Claude/Kiro guard).
- **Ollama:** model fallback to `"ollama"` instead of `undefined` in streamed chunks.
- **Error classification:** `capacity`/`overloaded` substring rules scoped to rate-limit statuses (was false-matching deterministic 400s like "context capacity exceeded").
- **Codex overloaded-detection:** `_peekSseOverloaded` acquires its reader lazily — no upstream socket leak on the retry path.

---

**Full diff:** `v0.5.15...v0.5.16`
