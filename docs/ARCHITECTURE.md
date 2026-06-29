# Briefing Arsitektur 9Router

> Untuk senior fullstack engineer yang baru bergabung. Dokumen ini hasil analisis terverifikasi tiap subsistem plus trace request end-to-end. Baca sekali, simpan sebagai peta.
>
> _Last updated: 2026-06-29 — semua path relatif terhadap root repo. Versi sebelumnya (2026-02-06) menggambarkan persistence sebagai `db.json` (JSON); itu sudah usang — store sekarang SQLite (`data.sqlite`), lihat §4.3._

---

## 1. Apa itu 9Router

9Router adalah **LLM proxy/router multi-provider yang berjalan lokal**. Ia menerima request gaya OpenAI / Anthropic / Gemini dari klien (CLI coding tools, IDE, app), me-resolve target provider+model, menerjemahkan request ke wire format upstream, men-dispatch lewat executor per-provider (auth, URL, retry, token refresh), lalu streaming/buffering response balik — diterjemahkan ke format yang klien harapkan. Satu kontrak (`handleChatCore`) menormalkan ~40 provider (Codex, Cursor, Gemini-CLI, Kiro, GitHub Copilot, gateway openai/anthropic-compatible, dst).

**Masalah yang dipecahkan:** kamu punya banyak akun/langganan LLM yang tersebar (Claude Pro, Codex, Copilot, free tiers). 9Router menyatukannya di balik satu endpoint OpenAI-compatible, dengan fallback otomatis antar-akun dan antar-model, kompresi token, dan — lewat MITM — bahkan membajak tool pihak ketiga (Antigravity/Copilot/Kiro/Cursor) agar transparan jalan di provider pool milikmu sendiri.

---

## 2. Arsitektur Tingkat Tinggi

Dua paruh yang terhubung di **satu seam tunggal**:

- **Next.js layer** (`src/`) — HTTP boundary + dashboard admin + persistence + MITM. Route handler LLM tipis (~30 baris), semua delegasi ke `handleChat`.
- **open-sse engine** (`open-sse/`) — pipeline provider-agnostic. Inti produk. Bisa di-bundle terpisah; Next.js hanya consumer-nya.

Seam tunggal: `src/sse/handlers/chat.js` → `handleChatCore()` dari `open-sse/handlers/chatCore.js`.

```
                          CLIENT (CLI / IDE / app)
                                   │  POST /v1/chat/completions | /v1/messages | /v1/responses
                                   ▼
                    ┌──────────────────────────────┐
                    │ custom-server.js             │  socket IP, strip x-9r-*, stamp real-ip
                    └──────────────┬───────────────┘
                                   ▼
                    next.config rewrite  /v1|/v1beta|/codex|/responses → /api/v1*
                                   ▼
                    ┌──────────────────────────────┐
                    │ proxy.js → dashboardGuard    │  authz: public LLM | admin deny-default | local-only
                    └──────────────┬───────────────┘
                                   ▼
                    app/api/v1/.../route.js  (ensureInitialized → handleChat)
                                   ▼
   ╔═══════════════════════════════════════════════════════════════════════════╗
   ║ src/sse/handlers/chat.js                                                   ║
   ║  handleChat → combo? → handleSingleModelChat                              ║
   ║   while(true){ getProviderCredentials → checkAndRefreshToken → ───┐       ║
   ║                                                                   │       ║
   ╚═══════════════════════════════════════════════════════════════════╪═══════╝
                                                                        ▼  SEAM
   ╔═══════════════════════════════════════════════════════════════════════════╗
   ║ open-sse/handlers/chatCore.js  handleChatCore()                            ║
   ║  detectFormat → resolveTransport → stream-decision                        ║
   ║  passthrough? {…body,model} : translateRequest(src→openai→tgt)            ║
   ║  RTK → Headroom → Caveman → Ponytail   (mutate translatedBody, fail-open) ║
   ║  getExecutor(provider).execute() ─────────────► UPSTREAM PROVIDER         ║
   ║  401/403 → refreshWithRetry(...,3) → re-execute once                      ║
   ║  dispatch: forcedSSE→JSON | nonStreaming | streaming(translateResponse)   ║
   ╚═══════════════════════════════════════════════════════════════════╤═══════╝
                                                                        ▼
   result.success? clearAccountError : markAccountUnavailable + excludeSet + loop
                                   ▼
                          SSE / JSON Response → CLIENT
```

---

## 3. open-sse engine (deep dive)

Inti 9Router. **Catatan koreksi penting:** `open-sse/index.js` BUKAN entry contract — itu cuma barrel re-export. Entry sebenarnya adalah `handleChatCore` di `open-sse/handlers/chatCore.js`. `chat.js` import `open-sse/index.js` hanya untuk side-effect (registrasi translator).

### 3.1 Lifecycle (`handlers/chatCore.js`)

Satu fungsi `handleChatCore()` (~300 baris, 30+ param ter-destructure) menjalankan seluruh siklus:

1. `sourceFormat = sourceFormatOverride || detectFormat(body)` — heuristik shape/endpoint.
2. Resolve alias → targetFormat + transport (`resolveTransport`), stripList, upstreamModel; inject provider thinking override.
3. **Stream-decision** (sumber kompleksitas): `clientRequestedStreaming` (body.stream / gemini source) × `providerRequiresStreaming` (forceStream) × Accept header × quirk tool/imageGen.
4. Strip modality tak didukung + prefetch remote images (kecuali passthrough).
5. Build outbound: passthrough → `{...body, model:upstreamModel}`; else `translateRequest(src→tgt)` (return `_toolNameMap`).
6. Token-saver chain pada `translatedBody`.
7. `getExecutor(provider).execute(...)` → raw upstream Response.
8. 401/403 (jika `!noAuth`) → `refreshWithRetry(()=>refreshCredentials,3)` → persist via `onCredentialsRefreshed` → re-execute **satu kali**.
9. `!ok` → `parseUpstreamError` → error result (+ `resetsAtMs` untuk 429).
10. Dispatch ke salah satu dari **tiga path response** (forcedSSE→JSON | non-streaming | streaming).

### 3.2 Executors (`executors/`)

- **Registry** (`executors/index.js`): ~24 executor ter-instantiate eager, keyed provider id (+alias `cu`→cursor, `mmf`→MimoFree, `vertex-partner`). `getExecutor()` → spesialis atau `DefaultExecutor` ter-cache lewat `defaultCache` Map.
- **`base.js` `BaseExecutor.execute()`**: loop dispatch universal — iterasi baseUrls (fallback), tiap iterasi panggil `buildUrl → transformRequest → buildHeaders` (**urutan ini load-bearing**), fetch dengan connect-timeout AbortController, retry config + `shouldRetry`. Return `{response,url,headers,transformedBody}`.
- **`default.js` `DefaultExecutor`**: config-as-code. Auth descriptor (BEARER/XAPIKEY/per-provider), OAuth refresh grant, header hook (kimi/cline/kilocode/claude) — semua diturunkan dari registry `PROVIDERS` saat module load. Branch `openai-compatible-*`/`anthropic-compatible-*` di `buildUrl`. **Inilah alasan provider OpenAI-compat generic tak butuh class executor sendiri.**

**Yang mem-wrap `execute()`:** Codex, Cursor, GitHub, Kiro, Grok, Mimo. Codex paling rumit — `_peekSseOverloaded()` membaca 4KB pertama body 200-OK untuk deteksi `server_is_overloaded`, lalu retry seakan 503 dengan merakit ulang ReadableStream dari prefix chunk yang sudah dikonsumsi + sisa upstream (fragile, untested vs backpressure).

> ⚠️ **Race trap:** beberapa executor menyimpan state di field instance (`_currentSessionId`, `_isCompact`, cached model) pada **singleton** yang di-share antar request konkuren. Dua request in-flight ke provider sama bisa cross-contaminate.

### 3.3 Translator (`translator/`)

Model **OpenAI-pivot hub**: OpenAI Chat = format intermediate tunggal. N dialek butuh ~2N adapter (to/from OpenAI), bukan N².

- **Self-registration**: tiap file translator panggil `register(from,to,reqFn,resFn)` saat import. `index.js` import statik hanya untuk trigger side-effect — file baru WAJIB di-import di sana. Dua registry: request keyed `source:target`, response keyed `target:source` (terbalik — mudah salah baca).
- **Direct route vs double-hop**: jika ada `src:tgt` lossless (cuma `claude:kiro` yang fully-direct di request side) → short-circuit. Else hop `src→openai→tgt`.
- **Unified thinking** (`concerns/thinkingUnified.js`): `captureThinking` snapshot intent dari body ASLI sebelum mutasi; `applyThinking` re-emit di dialek native target (~13 encoding, driven by `capabilities.js`).
- `filterToOpenAIFormat` (target=openai), `prepareClaudeRequest` (target=claude), OAuth tool cloaking.

> ⚠️ **Lossy bridge:** round-trip A→openai→A **bukan** identity. Hilang/degrade: thinking signature, cache_control, `tool_result.is_error`, document/non-image media, field safety/grounding Gemini. `kiro-to-claude` duplikasi ~150 baris `openai-to-claude` tapi tanpa `sanitizeToolArgs` — akan divergen. Untuk pasangan fragile, prefer direct route.

### 3.4 RTK token saver (`rtk/`)

Kompresor in-place pada `translatedBody`, dipanggil di `chatCore.js` (~line 159-186) **SETELAH** `translateRequest` (komentar `rtk/index.js:2` yang bilang "before translation" sudah stale/salah). Empat saver berurutan, semua **fail-open** (error → return null, body asli utuh; jangan throw):

1. **RTK** (`compressMessages`) — filter lossless tool-output (gitDiff/grep/ls/tree/find/dedup/smart-truncate), autodetect 1KB pertama → `safeApply`. Invariant never-grow ditegakkan di caller (`compressText` revert jika output ≥ input). Skip `is_error`/`status:"error"` untuk jaga trace.
2. **Headroom** — proxy eksternal `/v1/compress` (await di hot path, timeout 3s). Translate Claude/Responses→OpenAI dan balik. `isHeadroomPhantomSavings` deteksi token-win yang tak menyusutkan payload.
3. **Caveman** — system prompt terse.
4. **Ponytail** — system prompt lazy-senior-dev.

Injeksi Claude disisipkan **sebelum** `cache_control` terakhir agar masuk cached prefix (tak bust prompt caching).

> ⚠️ Klaim README "save 20-40% / 65%" **bukan dari kode** — RTK hanya ukur BYTES, bukan token. Treat sebagai ilustratif.

### 3.5 Providers & config (`providers/`)

Single-source registry: tiap provider satu file `providers/registry/{id}.js` (identity + transport + oauth + models + media). Build loop (`providers/index.js`) fan-out ke `PROVIDERS`/`PROVIDER_MODELS`/`PROVIDER_OAUTH`/`PROVIDER_MEDIA`. ~96 entry, ~398 LLM models, ~284 media.

- **Capabilities** (`capabilities.js`): resolusi 4-step — provider-override → exact → glob pattern → default floor. Sumber: models.dev.
- **Pricing** (`pricing.js`): 3-step. Strip vendor prefix sebelum lookup.
- ⚠️ `schema.js` `resolveProvider`/`ENDPOINT_DEFAULTS` adalah **dead code** — build asli pakai loop ringan `index.js` yang cuma re-apply `format`. Edit `schema.js` defaults = no-op runtime.

### 3.6 Services: fallback / auth / token-refresh

Dua level fallback yang nesting:
- **Intra-provider** (`src/sse/handlers/chat.js` while-loop): exclude connectionId gagal, retry akun berikutnya.
- **Inter-model combo** (`open-sse/services/combo.js`): jalan alias chain. Tiap model combo memanggil full account loop.

3-tier subscription→cheap→free adalah **emergent** dari urutan `models[]` di combo, bukan fitur hardcoded. FREE_PROVIDERS dapat virtual connection `noauth`.

- **Per-MODEL locking** (bukan per-account): `markAccountUnavailable` tulis `modelLock_<model>`. Akun rate-limited di satu model tetap layani model lain.
- **Round-robin sticky**: `getProviderCredentials` di-serialize lewat **global `selectionMutex`** (bottleneck process-wide).
- **Refresh concurrency**: `dedupRefresh` (cache 10s by `provider:oldToken`) + `withCredentialRefreshLock` (mutex per provider:connection) cegah `refresh_token_reused`.
- **Identity spoofing**: Cursor checksum (Jyh cipher, reverse-engineered), Cline headers (`workos:` bearer — tapi `X-CLIENT-TYPE:'9router'` membocorkan disguise), real GCP projectId (`projectId.js`) untuk hindari anti-abuse Google.

> **Koreksi:** 401/403 path = `refreshWithRetry(...,3)` (sampai 3x refresh) + 1x re-execute, bukan "refresh once".

---

## 4. Next.js Layer (`src/`)

### 4.1 API boundary

Next 16, `output: standalone`. Route LLM tipis (OPTIONS CORS + POST delegate). `detectFormatByEndpoint(pathname)` membedakan dialek: `/v1/messages`=Claude, `/v1/responses`=openai-responses, `/v1/chat/completions`=openai. Gemini punya catch-all `v1beta/models/[...path]/route.js` dengan transform bidirectional SSE/JSON.

- **`custom-server.js`**: derive IP dari socket TCP (unspoofable), percaya XFF hanya dari loopback, strip+re-stamp `x-9r-*`. Mengalahkan XFF spoofing.
- **`dashboardGuard.js`**: klasifikasi path — PUBLIC (LLM), ALWAYS_PROTECTED (JWT/CLI), LOCAL_ONLY (spawn/secret), deny-by-default untuk `/api/*`. **Auth split-brain:** proxy anggap `/v1*` public, enforcement key ada di `handleChat` (`settings.requireApiKey`). Jika `requireApiKey=false` + exposed > loopback → unauthenticated bisa pakai LLM API.

Route grup penting:
- Compatibility: `src/app/api/v1/{chat/completions,messages,responses,models}/route.js`, `messages/count_tokens`, `v1beta/models/[...path]`.
- Management: `auth/*`, `settings/*`, `providers*`, `provider-nodes*`, `oauth/*`, `keys*`, `models/alias`, `combos*`, `pricing`, `usage/*`, `sync/*`+`cloud/*`, `cli-tools/*`.

### 4.2 Dashboard UI (`src/app/(dashboard)/`)

Route group, **semua page `'use client'`**, raw `fetch` ke `/api/*` co-located. Zustand hanya untuk cross-cutting (toast/search/theme), bukan server data. Lib berat di-isolasi per-page: Monaco (translator), xyflow (usage topology, read-only), recharts (usage chart). `providerStore.js`, `settingsStore.js`, `shared/utils/api.js` = **dead code** (zero importer) — tiap page duplikasi fetch boilerplate.

### 4.3 Persistence (`src/lib/db/`)

> **Penting:** `src/lib/localDb.js` & `src/lib/usageDb.js` sekarang cuma **shim re-export** ke `src/lib/db/index.js`. Store sebenarnya **SQLite** (`${DATA_DIR}/db/data.sqlite`), bukan JSON. File `db.json`/`usage.json` lama hanya sumber migrasi legacy.

SQLite runtime-agnostic, single `data.sqlite`, adapter pluggable:
- **4-tier driver** (`driver.js`): bun:sqlite → better-sqlite3 → node:sqlite (≥22.5) → sql.js WASM.
- **Hybrid storage**: hot field = kolom, sisanya (termasuk **credential**) = JSON blob di kolom `data`.
- **Migrasi additive** (`migrate.js`): `TABLES` source of truth, auto ALTER ADD COLUMN; destruktif butuh migration manual.
- **Repo layer** (`src/lib/db/repos/`): settings, connections, nodes, proxyPools, apiKeys, combos, alias, pricing, usage, requestDetails, disabledModels.
- **Scoped KV**: satu tabel `kv(scope,key,value)` untuk alias/pricing/customModels.

Entitas inti: `PROVIDER_CONNECTION` (provider, authType, priority, isActive, apiKey, accessToken, refreshToken, expiresAt, testStatus, rateLimitedUntil, providerSpecificData), `PROVIDER_NODE`, `MODEL_ALIAS`, `COMBO`, `API_KEY`, `USAGE_ENTRY`, `SETTINGS`.

> ⚠️ **Token OAuth, API key provider, OIDC secret semua PLAINTEXT.** AES-GCM cuma untuk sudo password MITM. `exportDb` = full credential dump.

### 4.4 MITM proxy (`src/mitm/`)

HTTPS interception standalone privileged. DNS-hijack host tool AI (Antigravity/Copilot/Kiro/Cursor) → `127.0.0.1:443`, forge leaf cert dari self-signed Root CA terinstall di OS trust store, re-route request chat ke 9Router lokal.

- `manager.js` (parent): elevasi sudo/admin, gen+trust Root CA, spawn `server.js` as root, AES-256-GCM encrypt sudo pwd (key dari machine-id), crash auto-restart.
- `server.js` (:443): per-SNI leaf cert, ALPN h2/http1.1, model extraction, dispatch handler. Resolve upstream lewat 8.8.8.8 (hindari loop hosts-file).
- **Process-boundary read-replica**: MITM proses terpisah tanpa DB binding; alias lewat `aliases.json` yang di-sync atomik dari `kv['mitmAlias']` (`mitmAliasCache.js` → `dbReader.js`).
- Selective interception: hanya host+pattern+mapped-model di-route; autocomplete latency-kritis (`/^tab[_-]/`) passthrough.

> ⚠️ Root CA 10-tahun di OS trust store; jika `rootCA.key` (ditulis tanpa mode 0600 eksplisit) bocor → forge cert untuk SITUS APAPUN. Teardown tak lengkap (kill -9) bisa tinggalkan hosts entry + rogue CA terpercaya.

---

## 5. Distribusi

Satu app Next.js, dua jalur:

**npm global** (`cli/`): `cli.js` (~831 baris, pure stdlib) supervise Next standalone yang sudah di-build (`cli/app/`). Native deps (better-sqlite3, systray) **sengaja di luar tarball** → lazy-install ke `~/.9router/runtime` (hindari Windows EBUSY lock + AV false-positive).
- Build: `build-cli.js` (Next standalone, workspace tracing) → `buildMitm.js` (esbuild MITM jadi satu file zero-external, biar bisa di-spawn bebas node_modules lock).
- Validasi binary by magic bytes (ELF/Mach-O/PE). sql.js `.wasm` re-validate runtime (npm publish strip `.wasm` nested).
- Tray: Windows→PowerShell NotifyIcon (no binary, AV-safe); Unix→systray2. Autostart: launchd/Startup `.vbs`/`.desktop`.
- Crash supervisor: setelah MAX_RESTARTS, set `mitmEnabled=false` (MITM prime suspect) lalu restart.

**Docker/CapRover**: `Dockerfile` multi-stage node:22-alpine, `CMD node custom-server.js` port 20128. `docker-compose.yml` + headroom sidecar.

> ⚠️ Default bind `0.0.0.0` — hanya warning kuning. Provider credentials exposed ke LAN out-of-the-box.

---

## 6. Cloud Sync (opsional)

Sinkronisasi state multi-device lewat `NEXT_PUBLIC_CLOUD_URL` (implementasi cloud out-of-scope repo ini):

- Scheduler init: `src/lib/initCloudSync.js`, `src/shared/services/initializeCloudSync.js`; periodic task: `src/shared/services/cloudSyncScheduler.js`; control route: `src/app/api/sync/cloud/route.js`.
- **enable** → set `cloudEnabled=true`, ensure API key, `POST /sync/{machineId}` (providers/aliases/combos/keys), `GET /{machineId}/v1/verify`.
- **sync** → push/pull `POST /sync/{machineId}`, update token/status lokal yang lebih baru.
- **disable** → `cloudEnabled=false`, `DELETE /sync/{machineId}`, balikin `ANTHROPIC_BASE_URL` ke lokal bila perlu.
- Degradasi: error sync di-surface tapi runtime lokal tetap jalan.

---

## 7. Pola Desain Kunci

| Pola | Manifestasi |
|---|---|
| **Single-source registry** | `providers/registry/{id}.js` → build loop fan-out; appConstants derive fingerprint/OAuth/region dari registry |
| **Config-as-code** | `DefaultExecutor` turunkan auth/refresh dari registry, no per-provider class untuk OpenAI-compat |
| **OpenAI-pivot hub** | ~2N adapter, bukan N² |
| **Self-registering translators** | `register()` via import side-effect; `var` (bukan `let`) hindari TDZ saat circular import |
| **Layered fallback resolution** | capabilities (4-step) & pricing (3-step), first-match, order-sensitive |
| **Fail-open mutations** | RTK/Headroom/Caveman/Ponytail mutate in-place, error → body asli utuh |
| **Two-level nested fallback** | account loop × combo chain |
| **Per-model locking** | `modelLock_<model>` flat field, bukan sub-table |
| **Capture-before-mutate** | thinking intent di-snapshot sebelum body translation merusak field |
| **Stall watchdog on raw bytes** | timer pada upstream bytes, bukan transform output (reasoning stream silent) |
| **Process-boundary read-replica** | MITM baca alias JSON yang di-sync dari SQLite |
| **4-tier graceful degradation** | SQLite driver fallback sampai pure-JS WASM |

---

## 8. Risiko & Tech Debt (ranked by severity)

### 🔴 Kritis (keamanan / kehilangan data)
1. **Credential plaintext at rest** — token OAuth, API key provider, idToken, OIDC secret semua plaintext di `data.sqlite`. `exportDb` = dump kredensial. AES cuma untuk sudo pwd. (`db/repos/connectionsRepo.js`, `settingsRepo.js`)
2. **MITM Root CA blast radius** — Root CA 10-tahun di OS trust; `rootCA.key` tanpa 0600 eksplisit. Bocor = forge cert situs apapun. Teardown tak lengkap tinggalkan rogue CA + hosts entry. (`mitm/cert/rootCA.js`, `mitm/manager.js`)
3. **Inbound API key tidak di-hash** — disimpan raw, compare by equality. CRC integrity-only. `API_KEY_SECRET` default hardcoded. (`db/repos/apiKeysRepo.js`)
4. **Default & fallback secrets lemah** — password dashboard default `123456`; `ENCRYPT_SALT`/`API_KEY_SECRET` fallback konstanta prediktabel.
5. **Auth split-brain + 0.0.0.0 default** — `requireApiKey=false` + exposure beyond loopback → LLM API terbuka. Default bind LAN-exposed. (`dashboardGuard.js`, `cli/cli.js`)

### 🟠 Tinggi (correctness / reliability)
6. **Global `selectionMutex`** — serialize SEMUA `getProviderCredentials` process-wide; satu DB write lambat di critical section stall semua traffic. (`src/sse/services/auth.js`)
7. **Singleton executor state race** — `_currentSessionId`/`_isCompact`/cached model di instance shared, request konkuren cross-contaminate. (`executors/index.js`)
8. **No inbound rate limiting di LLM path** — satu klien noisy bisa rate-lock akun untuk semua (shared cooldown state). Hanya login lockout yang ada. (`loginLimiter.js`)
9. **401/403 retry pada `stream=true`** — streaming POST re-fired setelah refresh; bisa double-charge, drop header/transformedBody baru. (`chatCore.js`)
10. **Lossy OpenAI bridge** — round-trip non-identity; thinking signature/cache_control/is_error hilang di double-hop. (`translator/formats/openai.js`)
11. **No test CI** — ~949 test tidak terhubung ke CI; Docker publish pada tag tanpa gate test/lint. UI 100% untested. (`.github/workflows/docker-publish.yml`)

### 🟡 Sedang (maintainability)
12. **Codex `_peekSseOverloaded`** — replay prefix chunk + handoff reader fragile, untested vs backpressure, bisa silent truncate. (`executors/codex.js`)
13. **Glob-pattern ordering** = single point of correctness capabilities & pricing; tabel manual sudah drift tanpa guard. (`capabilities.js`, `pricing.js`)
14. **Format detection heuristik** — `needsTranslation` bare `!==`; mis-detect → garbled output bukan error. (`services/provider.js`)
15. **Identity spoofing fragile + ToS risk** — Cursor checksum/Cline header/projectId; `X-CLIENT-TYPE:'9router'` bocorkan disguise Cline. Banyak provider `deprecated:'RISK_NOTICE'`.
16. **Dead code & schema duplikasi** — `schema.js resolveProvider`, `providerStore`/`settingsStore`/`utils/api.js` zero-importer.
17. **sql.js full-DB-rewrite-on-debounce** — tak scale dengan growth usageHistory; kill dalam 100ms window = lost writes (no WAL). (`db/adapters/sqljsAdapter.js`)
18. **Lossy compression model-visible context** — filter truncate; jika model butuh bagian terbuang, kualitas turun silent. `is_error` gating shape-specific (OpenAI tool tak punya error flag). (`rtk/`)

---

## 9. Peta File untuk Navigasi Cepat

| Kalau mau ngubah... | Buka file |
|---|---|
| **Logika orkestrasi request inti** | `open-sse/handlers/chatCore.js` |
| **Seam Next→engine, account fallback loop** | `src/sse/handlers/chat.js` |
| **Resolusi alias/model `provider/model`** | `open-sse/services/model.js` |
| **Tambah provider baru** | `open-sse/providers/registry/{id}.js` (+ template `REGISTRY_TEMPLATE.js`) |
| **Logika executor generic (OpenAI-compat)** | `open-sse/executors/default.js` |
| **Loop fetch/retry/fallback executor** | `open-sse/executors/base.js` |
| **Daftar mapping executor spesialis** | `open-sse/executors/index.js` |
| **Translator request (Claude↔OpenAI dsb)** | `open-sse/translator/request/*.js` |
| **Translator response (SSE chunk)** | `open-sse/translator/response/*.js` |
| **Registry + pivot/direct routing translator** | `open-sse/translator/index.js` |
| **Normalisasi thinking/reasoning** | `open-sse/translator/concerns/thinkingUnified.js` |
| **Capability model (vision/thinking/limit)** | `open-sse/providers/capabilities.js` |
| **Harga model ($/1M token)** | `open-sse/providers/pricing.js` |
| **RTK compressor + token-saver wiring** | `open-sse/rtk/index.js` (filter di `rtk/filters/`) |
| **Tuning cap RTK** | `open-sse/rtk/constants.js` |
| **Aturan fallback/cooldown/backoff** | `open-sse/config/errorConfig.js` |
| **Pemilihan akun + model-lock** | `src/sse/services/auth.js` |
| **Combo (fallback/round-robin/fusion)** | `open-sse/services/combo.js` |
| **Token refresh per-provider** | `open-sse/services/tokenRefresh/providers.js` |
| **Path streaming (pipe/transform)** | `open-sse/handlers/chatCore/streamingHandler.js` |
| **Route LLM (OpenAI/Claude/Responses)** | `src/app/api/v1/{chat/completions,messages,responses}/route.js` |
| **Gemini-native compatibility** | `src/app/api/v1beta/models/[...path]/route.js` |
| **Authz proxy / klasifikasi path** | `src/dashboardGuard.js` |
| **Hardening IP / strip XFF** | `custom-server.js` |
| **Rewrite path + standalone config** | `next.config.mjs` |
| **Barrel + skema DB** | `src/lib/db/index.js`, `src/lib/db/schema.js` |
| **Pemilihan driver SQLite** | `src/lib/db/driver.js` |
| **Penyimpanan credential provider** | `src/lib/db/repos/connectionsRepo.js` |
| **Settings + default** | `src/lib/db/repos/settingsRepo.js` |
| **MITM lifecycle (CA, sudo, spawn)** | `src/mitm/manager.js` |
| **MITM server interception :443** | `src/mitm/server.js` |
| **Daftar host tool yang dibajak** | `src/shared/constants/mitmToolHosts.js` |
| **Handler Kiro (AWS EventStream)** | `src/mitm/handlers/kiro.js` |
| **Cloud sync scheduler/control** | `src/shared/services/cloudSyncScheduler.js`, `src/app/api/sync/cloud/route.js` |
| **Launcher npm CLI** | `cli/cli.js` |
| **Build pipeline distribusi** | `cli/scripts/build-cli.js` |
| **Lazy-install runtime deps** | `cli/hooks/sqliteRuntime.js`, `cli/hooks/trayRuntime.js` |
| **Navigasi/feature-flag UI** | `src/shared/components/Sidebar.js` |
| **Detail provider (section terbesar UI)** | `src/app/(dashboard)/dashboard/providers/[id]/page.js` |
| **Runner test** | `tests/vitest.config.js` (jalankan dari `tests/`, butuh `NODE_PATH=/tmp/node_modules`) |
| **Konvensi test translator + katalog bug** | `tests/translator/AGENTS.md` |

---

## 10. Environment & Runtime Matrix

- App/auth: `JWT_SECRET`, `INITIAL_PASSWORD` (default `123456` — WAJIB override)
- Storage: `DATA_DIR` (default `~/.9router`)
- Security hashing: `API_KEY_SECRET`, `MACHINE_ID_SALT`, `ENCRYPT_SALT`
- Logging: `ENABLE_REQUEST_LOGS` (tulis full header/body — direktori log = sensitif)
- Sync/cloud: `NEXT_PUBLIC_BASE_URL`, `NEXT_PUBLIC_CLOUD_URL`
- Outbound proxy: `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`/`NO_PROXY` (+ lowercase) via `open-sse/utils/proxyFetch.js`
- Platform: `PORT` (default 20128), `HOSTNAME`, `NODE_ENV`, `APPDATA`

---

**Saran onboarding praktis:** mulai dari trace di §2, baca `chatCore.js` sambil buka `chat.js` di sebelahnya — itu 80% mental model. Jangan percaya komentar in-file di `rtk/index.js:2` dan `open-sse/index.js` (keduanya menyesatkan; lihat koreksi §3). Sebelum sentuh executor, ingat singleton state race (§8.7). Sebelum demo ke publik, audit §8 item 1-5.
