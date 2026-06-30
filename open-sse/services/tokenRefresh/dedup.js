import { AsyncLocalStorage } from "node:async_hooks";

const REFRESH_RESULT_TTL_MS = 10_000;
const refreshDedupCache = new Map();

// Per-call AsyncLocalStorage carrying the connectionId of the credential being refreshed.
// Two distinct connections can legitimately share a refresh-token value (e.g. the same OAuth token
// imported into two connection records), and providers that ROTATE refresh tokens (codex, claude,
// github) return a new token persisted to one connection while the other's later refresh uses its
// stale oldToken. The old `provider:oldToken` dedup key collapsed both connections into one cache
// entry, handing one connection's rotated result to the other — desyncing their refresh tokens or
// failing with refresh_token_reused. Scoping the key per connection (via this store, set by
// refreshProviderCredentials which always has the full credentials) prevents that cross-connection
// bleed while still deduping concurrent refreshes for the SAME connection.
const refreshCtx = new AsyncLocalStorage();

export function runRefreshContext(connectionId, fn) {
  return refreshCtx.run({ connectionId }, fn);
}

export async function dedupRefresh(provider, oldToken, fn, log, connectionId = null) {
  if (!oldToken) return fn();
  // Prefer an explicit connectionId arg; fall back to the ambient one set by
  // refreshProviderCredentials. Both keep the dedup key scoped per connection.
  const connId = connectionId || refreshCtx.getStore()?.connectionId || null;
  const key = connId ? `${provider}:${connId}:${oldToken}` : `${provider}:${oldToken}`;
  const hit = refreshDedupCache.get(key);
  if (hit) {
    if (hit.promise) {
      log?.info?.("TOKEN_REFRESH", `Reusing in-flight refresh for ${provider}`);
      return hit.promise;
    }
    if (hit.expiresAt > Date.now()) {
      log?.info?.("TOKEN_REFRESH", `Reusing recent refresh result for ${provider}`);
      return hit.result;
    }
    refreshDedupCache.delete(key);
  }
  const promise = (async () => {
    try {
      const result = await fn();
      // Provider refresh functions resolve to null on transient failures (network blip, upstream
      // 5xx) rather than throwing. Caching that null for 10s would take the whole connection
      // offline even though the very next attempt would likely succeed. Only cache a genuine
      // (non-null) refresh result; a null result leaves the key un-cached so callers retry.
      if (result) {
        refreshDedupCache.set(key, { result, expiresAt: Date.now() + REFRESH_RESULT_TTL_MS });
      } else {
        refreshDedupCache.delete(key);
      }
      return result;
    } catch (err) {
      refreshDedupCache.delete(key);
      throw err;
    }
  })();
  refreshDedupCache.set(key, { promise });
  return promise;
}
