# Sinkronisasi 9router fork `go` → upstream `v0.5.35`

- **Tanggal:** 2026-07-17
- **Branch sumber:** `go` (HEAD `193de26`, "feat(go): native /v1/chat/completions slice + bun runtime migration")
- **Upstream target:** `upstream/master` = `v0.5.35` (`bc252ea`, 2026-07-16)
- **Merge-base:** `9845a17` = `v0.5.30` (2026-07-10) — bersih, tidak ada commit upstream yang hilang selain 0.5.31–0.5.35
- **Pendekatan:** `git merge upstream/master` ke `go` (Pendekatan A)
- **Tujuan:** menyatukan perbaikan/fitur upstream 0.5.31–0.5.35 ke fork, **tanpa menghapus atau menonaktifkan fitur lokal apa pun**, lalu membangun ulang paket global `9router` dari fork.

## 1. Lingkup dan prinsip

### 1.1 Prinsip pelestarian

Operasi ini **tidak boleh menghapus, menonaktifkan, atau melebarkan cakupan** dari fitur lokal berikut. Jika sebuah konflik memaksa pilihan, prioritas pada sisi lokal kecuali eksplisit dinyatakan lain pada peta resolusi (§3):

- **Backend Go** (`backend/`): reverse-proxy bridge, native SQLite CRUD, JWT/CLI auth parity, `/v1/models` dengan embedded catalog snapshot, strict slice `/v1/chat/completions` (JSON + SSE), retry/moderation classification/model locks/usage writes.
- **AgentRouter provider**: registry `open-sse/providers/registry/agentrouter.js`, CLI/TUI integration, connection-test probe.
- **ZCode integration**: BYOK guide (`cliTools.js`), Claude passthrough tool-name decloak (`chatCore.js`/stream), test `tests/unit/passthrough-tool-decloak.test.js`.
- **Moderation-aware fallback**: cross-provider reroute saat content-moderation block, no-lock on moderation rejection, tests `tests/unit/moderation-no-lock.test.js` + `tests/unit/moderation-reroute.test.js`.
- **Security/correctness patchset**: 22 bug (commit `519139a`) mencakup auth hardening, SQLite atomicity, token-refresh dedup per-connection, request isolation, backup completeness, stream fixes.
- **Runtime/build lokal**: `scripts/prod-local.sh`, `scripts/dev-all.js`, `Dockerfile` (Go+Node bridge), `docker-entrypoint.sh`, migrasi tooling ke Bun.
- **3 stash** (`stash@{0..2}`) dibiarkan apa adanya.

### 1.2 Yang dibawa upstream (26 commit, 89 file, +9841/−649)

Fitur/fix penting:

- `feat(xai)`: Grok Imagine video generation (`/v1/videos`) + CLI subcommand `9router xai video`.
- `feat(cli-tools)`: Grok Build setup (`grok-build` di `cliTools.js`, config `grokCli.js`).
- `feat(github)`: route Claude models melalui Copilot native `/v1/messages`.
- `feat(kiro)`: GPT-5.6 model family (`kiroConstants.js`).
- `feat(rtk)`: header `X-9Router-Token-Saver` untuk bypass token savers per-request.
- `feat`: provider quota visibility settings.
- `fix(anthropic)`: lowercase `anthropic-version` header key (cegah duplikasi di `/v1/messages`).
- `fix(alicode-intl)`: endpoint DashScope compatible-mode.
- `fix(grok-cli)`: align Grok Build subscription protocol; surface `expiresAt` untuk proactive token refresh.
- `fix(kiro)`: direct session cache reuse (`kiroSessionReplay.js`).
- `fix(models)`: populate capabilities untuk live-catalog LLM models; list compatible provider models di `/v1/models`.
- `fix(thinking)`: kirim `thinking:{type:adaptive}` alongside `output_config.effort`.
- `fix(translator)`: strip `client_metadata` pada konversi openai-responses→openai; drop `temperature` untuk semua model Claude (bukan hanya opus-4).
- `fix(providers)`: bulk-add API keys tidak overwrite existing keys (`src/shared/utils/bulkAdd.js`).
- `perf(startup)`: skip inactive background services.
- `i18n`: Thai (1389 keys) + Persian (README + UI).

File upstream murni (74 file) auto-masuk tanpa konflik. Daftar lengkap pada `git diff --name-status v0.5.30 v0.5.35`.

## 2. Operasi inti

### 2.1 Pra-kondisi

- Working tree `go` bersih (verified: clean).
- `upstream/master` sudah di-fetch ke `v0.5.35` (done during design).
- Merge-base konfirmasi `9845a17` = `v0.5.30`.

### 2.2 Backup

```bash
git branch go-backup-presync-v0535 go
```

Branch backup ini tidak di-push kecuali diminta; menjadi rollback point.

### 2.3 Merge

```bash
git checkout go
git merge upstream/master
```

Diharapkan sebagian besar file auto-merge. Konflik hanya pada subset §3.

### 2.4 Resolusi konflik

Mengikuti peta resolusi §3, per file. Prioritas default: **kombinasi kedua sisi** (kedua sisi aditif pada region berbeda). Hanya `package.json`/`cli/package.json`/CHANGELOG/README yang butuh triase manual versi.

### 2.5 Commit merge

Setelah seluruh konflik selesai dan `git status` bersih (hanya file merge-related), `git commit --no-edit` (pesan default merge) atau pesan kustom:

```
Merge upstream v0.5.35 into go

Sync 0.5.31–0.5.35 (Grok Imagine video, Grok Build, GitHub native
/v1/messages, GPT-5.6, quota visibility, X-9Router-Token-Saver, i18n
Thai/Persian, translator/anthropic/kiro fixes) while preserving the Go
backend, AgentRouter, ZCode, moderation fallback, and the security
patchset.
```

## 3. Peta resolusi file tumpang-tindih

15 file disentuh kedua sisi (`comm -12` dari `git diff --name-only`). Klasifikasi:

### 3.1 Auto-merge (region berbeda, kedua sisi aditif)

| File | Lokal menambah | Upstream menambah | Tindakan |
|---|---|---|---|
| `open-sse/services/model.js` | `stripContextSuffix()` + 4 call site | `BUILTIN_MODEL_ALIASES` (grok-build) + fallback resolve | Gabung; keduanya di region berbeda. |
| `open-sse/translator/concerns/paramSupport.js` | `hasPotentialParamTransform()` export | Perluas rule `claude-opus-4`→`claude` untuk drop temperature | Gabung; keduanya. |
| `open-sse/translator/index.js` | `outputIndex` counter + helper fields di `initState` | Guard kiro-thinking di `translateRequest` | Gabung; keduanya. |
| `src/app/api/providers/[id]/models/route.js` | `agentrouter` config block | `grok-cli` customResolver + 2 import | Gabung; pertahankan import + kedua blok. |
| `src/lib/db/repos/settingsRepo.js` | `requireApiKey: true` | `quotaVisibility: {}` | Gabung; kedua key. |
| `open-sse/handlers/chatCore.js` | Drain 401 body sebelum discard reference | Refactor ~45 baris region lain | Inspeksi; kemungkinan auto-merge. Bila konflik, prioritaskan drain lokal. |

### 3.2 Auto-merge dengan verifikasi (entri terpisah di struktur data)

| File | Catatan |
|---|---|
| `src/shared/constants/cliTools.js` | Lokal: entri `zcode`. Upstream: entri `grok-build` + 3 model GPT-5.6 di `MITM_TOOLS.kiro`. Region berbeda; gabung keduanya. Verifikasi tidak ada duplikat id. |
| `cli/cli.js` | Lokal: ~125 baris (AgentRouter TUI, launch changes). Upstream: blok subcommand `xai video` + help text di region awal. Kemungkinan auto-merge; verifikasi. |
| `.gitignore` | Lokal: `/bin/`, `/backend/bin/`, `*.test`. Upstream: `.claude/`, `.docs/`, `.repo/`, `.script/`, `.codegraph/`, `.PR`. Region berdekatan; gabung kedua set. |

### 3.3 Konflik manual (version bump vs scripts lokal)

| File | Lokal | Upstream | Resolusi |
|---|---|---|---|
| `package.json` | scripts→bun, `dev:backend`, `dev:all`, `build:backend`, `test:backend`, `gen:models-snapshot`, `verify:models-parity`, `prod:local`, `trustedDependencies`, `packageManager`, `engines` | `version: 0.5.30`→`0.5.35` (hanya baris version) | Terima scripts/metadata lokal **+ bump version ke `0.5.35`**. |
| `cli/package.json` | scripts→bun (`bun run`, `bun pm pack`, `bun publish`) | `version: 0.5.30`→`0.5.35` | Terima scripts lokal **+ version `0.5.35`**. |
| `CHANGELOG.md` | Entri lokal (release notes fork) | Entri 0.5.31–0.5.35 | Manual: tambah entri upstream 0.5.31–0.5.35 di atas, pertahankan entri lokal di bawah. |
| `README.md` | Konten lokal | Link Persian/Thai tutorial | Manual: pertahankan lokal, integrasikan link/tutorial baru upstream. |

### 3.4 UI overlap (perlu inspeksi, kemungkinan coexist)

| File | Lokal | Upstream |
|---|---|---|
| `src/app/(dashboard)/dashboard/usage/components/ProviderLimits/index.js` | Collapsible quota tracker cards | Quota visibility wiring |
| `src/app/(dashboard)/dashboard/usage/components/ProviderLimits/utils.js` | (quota tracker helpers) | Quota visibility helpers |

Kedua sisi menyentuh area quota UI. Buka saat konflik; jika region berbeda, gabung. Jika overlap logika, pertahankan fungsionalitas lokal lalu integrasikan hook visibility upstream.

## 4. Build

### 4.1 Build source (dev)

```bash
bun install --frozen-lockfile
cd backend && go build ./cmd/server && cd ..
bun run build
```

Tidak ada perubahan pada konfigurasi build. File upstream baru (videoCore, grokCliModels, kiroSessionReplay, route `/v1/videos/*`) adalah JS murni dan ter-trace otomatis oleh Next.js standalone.

### 4.2 Model snapshot Go — regenerate + parity

Upstream mengubah katalog model (GPT-5.6 Kiro, Grok Build, capability populate). Snapshot Go di `backend/internal/httpapi/models.go` harus sinkron dengan registry JS:

```bash
bun run gen:models-snapshot    # regenerate dari open-sse registry terbaru
bun run verify:models-parity   # verifikasi snapshot == registry
```

Jika parity gagal, perbarui snapshot sebelum lanjut ke packaging.

### 4.3 Packaging global `9router` (dari fork)

Menggantikan `bun i -g 9router@latest` (registry upstream) dengan instalasi dari tarball fork:

```bash
cd cli
bun run build           # build-cli.js langkah 9: build app + cross-compile Go
                       #   → cli/app/bin/{linux,darwin}-{amd64,arm64}/9router-backend
cd ..
bun i -g 9router-0.5.35.tgz   # install tarball lokal, nama paket tetap `9router`
```

- Nama paket: tetap `9router` (kompatibilitas bin `9router`).
- Versi: `0.5.35` (sama dengan upstream metadata; isi = fork).
- Windows sengaja dikecualikan dari cross-compile Go (binary belum signed) — fallback ke Node-only, sesuai `nativeBackendPath()` yang return `null` di win32.

### 4.4 Catatan auto-updater

`src/lib/appUpdater.js` / `src/lib/updater/updater.js` menjalankan `bun i -g 9router` yang menarik registry upstream. Ini **tidak diubah** dalam sinkronisasi ini (di luar lingkup). Konsekuensi: auto-updater dapat menimpa fork dengan upstream. Mitigasi: dokumentasikan bahwa fork global di-install dan di-update manual dari tarball, dan nonaktifkan auto-update check saat runtime via flag CLI `--skip-update` jika perlu.

## 5. Testing

Urutan sebelum packaging. Setiap tes harus lulus (atau eksplisit diakui sebagai snapshot yang sengaja berubah).

| # | Test | Command | Kriteria lolos |
|---|---|---|---|
| 1 | Go unit | `cd backend && go test ./...` | 0 fail. Native slice tidak regress. |
| 2 | JS unit lokal | `bun test tests/unit/moderation-no-lock.test.js tests/unit/moderation-reroute.test.js tests/unit/passthrough-tool-decloak.test.js tests/unit/headroom-chat-core.test.js` | Fitur lokal utuh. |
| 3 | JS unit upstream baru | `bun test tests/unit/` (filter file baru 0.5.35: `xai-video`, `grok-cli-models`, `grok-cli-expiresat`, `provider-quota-visibility`, `bulk-add-names`, `alicode-intl-endpoint`, `cli-xai-video`) | Fitur upstream berfungsi. |
| 4 | Translator + golden | `bun test tests/translator/` | Snapshot tidak drift. Jika gagal pada rule `claude`/kiro-thinking, konfirmasi intentional lalu `bun test --update-snapshots`. |
| 5 | Smoke runtime | `scripts/prod-local.sh start`; `curl :21128/health`; `curl :21128/v1/models` | Stack Go+Node start bersih; Go front-door merespons. |
| 6 | Model parity | `bun run verify:models-parity` | Snapshot Go == registry JS. |

Tes #4 paling mungkin gagal: upstream mengubah rule temperature (`claude` vs `claude-opus-4`) dan kiro thinking guard. Snapshot golden request akan berubah. Putuskan per kasus: jika perubahan eksplisit dari upstream commit (`9173c29`, `ba508f2`), terima dan update snapshot.

## 6. Rollback

Bila merge tidak bisa diselesaikan dengan bersih atau tes kritis gagal tanpa jalan keluar:

```bash
git reset --hard go-backup-presync-v0535
```

Tidak ada data yang hilang; branch backup utuh, stash utuh, `upstream/master` tetap di `v0.5.35`.

## 7. Di luar lingkup

- Mengubah auto-updater agar menarik fork (butuh registry/private feed sendiri).
- Mempublikasikan fork ke npm registry (butuh akun/ scope; saat ini install dari tarball lokal).
- Membuka stash WIP (`stash@{0..2}`) — dibiarkan apa adanya.
- Merebase `go` ke atas `upstream/master` (ditolak: force-push ke cabang terpublikasi).
- Menambah fitur baru; ini murni sinkronisasi + build.
