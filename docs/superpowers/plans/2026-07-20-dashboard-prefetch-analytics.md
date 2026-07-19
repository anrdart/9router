# Dashboard Prefetch and Analytics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop speculative Sidebar route requests and make Google Analytics explicitly opt-in for self-hosted deployments.

**Architecture:** Preserve Next.js `Link` navigation while disabling prefetch only at the persistent Sidebar boundary. Keep Analytics in the server-rendered root layout, but gate rendering with `NEXT_PUBLIC_GOOGLE_ANALYTICS_ID`; an absent variable produces no Analytics component or client script.

**Tech Stack:** Bun 1.3, Next.js 16 App Router, React 19, `@next/third-parties`.

---

## File Structure

- Modify `src/shared/components/Sidebar.js`: disable prefetch on its seven `Link` render sites.
- Modify `src/app/layout.js`: replace hard-coded Analytics ID with an optional environment variable.
- Keep `package.json` and lockfiles unchanged.

### Task 1: Disable Sidebar Route Prefetch

**Files:**
- Modify: `src/shared/components/Sidebar.js:122-328`

- [ ] **Step 1: Capture the failing static invariant**

Run this read-only check before editing:

```bash
python3 - <<'PY'
from pathlib import Path
s = Path("src/shared/components/Sidebar.js").read_text()
assert s.count("<Link") == 7, "Sidebar Link count changed; inspect manually"
assert s.count("prefetch={false}") == 7, "not every Sidebar Link disables prefetch"
PY
```

Expected: FAIL with `AssertionError: not every Sidebar Link disables prefetch`.

- [ ] **Step 2: Add the minimal props**

Add exactly one `prefetch={false}` to each Sidebar `Link`:

```jsx
<Link href="/dashboard" prefetch={false} className="flex items-center gap-3">
```

For each multiline site, place it after `href`:

```jsx
<Link
  key={item.href}
  href={item.href}
  prefetch={false}
  onClick={onClose}
```

Apply the same placement to dynamic media-kind, combined web, system, debug, and profile links. Do not alter classes, children, hrefs, or handlers.

- [ ] **Step 3: Verify the invariant passes**

Run:

```bash
python3 - <<'PY'
from pathlib import Path
s = Path("src/shared/components/Sidebar.js").read_text()
assert s.count("<Link") == 7, "Sidebar Link count changed; inspect manually"
assert s.count("prefetch={false}") == 7, "not every Sidebar Link disables prefetch"
PY
```

Expected: exit 0 with no output.

### Task 2: Make Google Analytics Opt-in

**Files:**
- Modify: `src/app/layout.js:30-47`

- [ ] **Step 1: Capture the failing Analytics invariant**

Run before editing:

```bash
python3 - <<'PY'
from pathlib import Path
s = Path("src/app/layout.js").read_text()
assert "G-LC959F603F" not in s, "hard-coded Analytics ID remains"
assert "process.env.NEXT_PUBLIC_GOOGLE_ANALYTICS_ID" in s, "Analytics env gate missing"
assert "googleAnalyticsId &&" in s, "conditional Analytics render missing"
PY
```

Expected: FAIL with `AssertionError: hard-coded Analytics ID remains`.

- [ ] **Step 2: Add the server-safe environment gate**

At the start of `RootLayout`, read the public environment variable:

```jsx
export default function RootLayout({ children }) {
  const googleAnalyticsId = process.env.NEXT_PUBLIC_GOOGLE_ANALYTICS_ID;

  return (
```

Replace the unconditional component:

```jsx
{googleAnalyticsId && <GoogleAnalytics gaId={googleAnalyticsId} />}
```

Do not remove the import or dependency. React renders nothing for an absent value during server rendering, avoiding client-only state and hydration differences.

- [ ] **Step 3: Verify the invariant passes**

Run:

```bash
python3 - <<'PY'
from pathlib import Path
s = Path("src/app/layout.js").read_text()
assert "G-LC959F603F" not in s, "hard-coded Analytics ID remains"
assert "process.env.NEXT_PUBLIC_GOOGLE_ANALYTICS_ID" in s, "Analytics env gate missing"
assert "googleAnalyticsId &&" in s, "conditional Analytics render missing"
PY
```

Expected: exit 0 with no output.

### Task 3: Validate the Patch

**Files:**
- Verify: `src/shared/components/Sidebar.js`
- Verify: `src/app/layout.js`
- Verify unchanged: `package.json`, `bun.lock`

- [ ] **Step 1: Inspect the focused diff**

Run:

```bash
git diff -- src/shared/components/Sidebar.js src/app/layout.js package.json bun.lock
```

Expected: only seven `prefetch={false}` additions, one environment lookup, one conditional Analytics render. No dependency or lockfile diff.

- [ ] **Step 2: Run ESLint**

Run:

```bash
bunx eslint src/shared/components/Sidebar.js src/app/layout.js
```

Expected: exit 0. Report existing warnings separately; do not perform unrelated cleanup.

- [ ] **Step 3: Build production assets**

Run:

```bash
bun run build
```

Expected: Next.js production build exits 0.

- [ ] **Step 4: Review repository state and final diff**

Run:

```bash
git status --short
git diff --check
git diff -- src/shared/components/Sidebar.js src/app/layout.js
git diff --stat
```

Expected: no whitespace errors; only intended source edits plus the approved spec/plan documents.

### Task 4: Production and Browser Verification

**Files:**
- No source changes.

- [ ] **Step 1: Restart without duplicating listeners**

Inspect the project production script before using its supported stop/restart behavior. Start with the required environment only after ensuring ports `21128` and `21129` are not held by a duplicate instance:

```bash
WITH_HEADROOM=1 GO_PORT=21128 NODE_PORT=21129 bun run prod:local
```

If production is managed externally, use that manager instead. Do not kill unrelated processes.

- [ ] **Step 2: Verify local health**

Run:

```bash
curl -i http://127.0.0.1:21128/health
```

Expected: HTTP 200 and `{"ok":true}`.

- [ ] **Step 3: Verify public dashboard response**

Run:

```bash
curl -I https://router.ekalliptus.com/dashboard
```

Expected: a healthy application response. Authentication redirects are acceptable if the dashboard requires login; server errors are not.

- [ ] **Step 4: Verify Chrome Network behavior**

1. Open Chrome DevTools, select **Network**, enable **Disable cache**, clear requests.
2. Load `/dashboard` in the production deployment.
3. Filter `_rsc` and confirm Sidebar render alone does not request `providers`, `combos`, `endpoint`, or `usage` RSC payloads.
4. Click one Sidebar item; confirm its route request begins after the click and navigation succeeds.
5. Filter `G-LC959F603F`, `googletagmanager`, and `google-analytics`; with `NEXT_PUBLIC_GOOGLE_ANALYTICS_ID` absent, confirm no Analytics request.
6. Check Console for new errors and compare dashboard appearance before/after; expected: no visual difference.

## Completion Report

Report:

- Root cause with actual source locations.
- ESLint and build outcomes verbatim, including warnings.
- Health/public HTTP outcomes, or clearly state if runtime verification was skipped.
- Final `git diff` summary.
- Restart command and Chrome verification steps.
- Do not commit unless explicitly requested.
