import { AsyncLocalStorage } from "node:async_hooks";

// Per-request isolation for executor state.
//
// Executors are singletons (one cached instance per provider, see executors/index.js), shared
// across all concurrent requests in the process. Several subclasses previously stashed
// per-request values on `this.*` (e.g. `_currentModel`, `_isCompact`, `_currentSessionId`) and
// read them back in buildUrl/buildHeaders. Because `execute()` awaits a fetch across the
// transform→header sequence, two concurrent requests to the same provider interleaved and
// overwrote each other's `this.*` — session ids, compact flags and model identifiers leaked
// between requests (wrong prompt-cache key, wrong User-Agent, wrong upstream URL).
//
// AsyncLocalStorage gives each in-flight execute() its own context without changing any method
// signature: execute() stores the request-scoped state via runRequestContext(), and
// buildUrl/buildHeaders read it via getRequestContext(). No cross-request contamination is
// possible because each async chain has its own store.
const als = new AsyncLocalStorage();

// Run `fn` with a per-request context. Returns whatever fn returns.
export function runRequestContext(fn) {
  return als.run({}, fn);
}

// Mutate the current request context (called from execute() after the store is entered).
export function setRequestContext(updates) {
  const store = als.getStore();
  if (store) Object.assign(store, updates);
}

// Read a value from the current request context. Returns undefined outside a request context
// (e.g. in unit tests that call buildHeaders directly), so callers should keep sensible defaults.
export function getRequestContext() {
  return als.getStore() || EMPTY;
}

const EMPTY = Object.freeze({});
