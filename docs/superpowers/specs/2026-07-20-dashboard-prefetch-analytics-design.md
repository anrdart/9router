# Dashboard Prefetch and Analytics Design

## Goal

Reduce unnecessary dashboard startup requests without changing UI, routing, authentication, APIs, data, or backend behavior.

## Root Cause

`Sidebar` is rendered throughout the dashboard and contains persistent Next.js `Link` components without an explicit `prefetch` setting. In production, Next.js may prefetch visible internal destinations, producing unsolicited RSC requests for several dashboard routes.

The root layout always renders `GoogleAnalytics` with a hard-coded measurement ID, so every self-hosted deployment loads Google Analytics by default.

## Design

- Add `prefetch={false}` to every Next.js `Link` rendered by `src/shared/components/Sidebar.js`, including logo, primary, media, system, debug, and profile links.
- Keep `Link` and existing click/navigation behavior unchanged.
- Read `NEXT_PUBLIC_GOOGLE_ANALYTICS_ID` in `src/app/layout.js`.
- Render `GoogleAnalytics` only when that variable is non-empty; otherwise render nothing.
- Remove the hard-coded measurement ID.
- Keep `@next/third-parties`; no dependency changes.

## Scope Exclusions

- Do not disable prefetch globally.
- Do not alter links outside `Sidebar.js`.
- Do not refactor duplicate desktop/mobile Sidebar rendering or its settings/version fetches.
- Do not change visual styles, page structure, authentication, API endpoints, database, Go backend, Headroom, or production routing.

## Verification

1. Check every Sidebar `Link` has exactly one `prefetch={false}`.
2. Run ESLint against changed JavaScript files using the installed tooling.
3. Run `bun run build`.
4. Review the final diff for unrelated changes and duplicate props.
5. If the production process can be safely restarted, verify `/health` and public dashboard HTTP status.
6. In Chrome DevTools production Network, confirm Sidebar rendering no longer triggers destination RSC requests. Confirm requests occur after navigation.
7. With the analytics environment variable absent, confirm no request for `G-LC959F603F` or Google Tag Manager. With an explicit ID, confirm Analytics loads that ID.

## Trade-off

The first navigation to a Sidebar destination may begin loading only after click. This intentionally exchanges speculative bandwidth and server work for a quieter initial dashboard load.
