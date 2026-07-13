// OpenAI-compatible error types mapping (client-facing)
export const ERROR_TYPES = {
  400: { type: "invalid_request_error", code: "bad_request" },
  401: { type: "authentication_error", code: "invalid_api_key" },
  402: { type: "billing_error", code: "payment_required" },
  403: { type: "permission_error", code: "insufficient_quota" },
  404: { type: "invalid_request_error", code: "model_not_found" },
  406: { type: "invalid_request_error", code: "model_not_supported" },
  429: { type: "rate_limit_error", code: "rate_limit_exceeded" },
  500: { type: "server_error", code: "internal_server_error" },
  502: { type: "server_error", code: "bad_gateway" },
  503: { type: "server_error", code: "service_unavailable" },
  504: { type: "server_error", code: "gateway_timeout" }
};

// Default error messages per status code (client-facing)
export const DEFAULT_ERROR_MESSAGES = {
  400: "Bad request",
  401: "Invalid API key provided",
  402: "Payment required",
  403: "You exceeded your current quota",
  404: "Model not found",
  406: "Model not supported",
  429: "Rate limit exceeded",
  500: "Internal server error",
  502: "Bad gateway - upstream provider error",
  503: "Service temporarily unavailable",
  504: "Gateway timeout"
};

// Exponential backoff config for rate limits
export const BACKOFF_CONFIG = {
  base: 2000,
  max: 5 * 60 * 1000,
  maxLevel: 15
};

// Default cooldown for transient/unknown errors
export const TRANSIENT_COOLDOWN_MS = 30 * 1000;

// Hard cap for provider-reported rate limit cooldown (e.g. codex resets_at can be 5-6h)
export const MAX_RATE_LIMIT_COOLDOWN_MS = 30 * 60 * 1000;

// Cooldown durations (ms)
const COOLDOWN = {
  long: 2 * 60 * 1000,
  short: 5 * 1000,
};

/**
 * Unified error classification rules.
 * Checked top-to-bottom: text rules first (by order), then status rules.
 * Each rule: { text?, status?, cooldownMs?, backoff? }
 *   - text: substring match (case-insensitive) on error message
 *   - status: HTTP status code match
 *   - cooldownMs: fixed cooldown duration
 *   - backoff: true = use exponential backoff (rate limit)
 */
export const ERROR_RULES = [
  // --- Text-based rules (checked first, order = priority) ---
  { text: "no credentials",           cooldownMs: COOLDOWN.long },
  { text: "request not allowed",      cooldownMs: COOLDOWN.short },
  { text: "improperly formed request", cooldownMs: COOLDOWN.long },
  { text: "rate limit",               backoff: true },
  { text: "too many requests",        backoff: true },
  { text: "quota exceeded",           backoff: true },
  // "capacity"/"overloaded" are rate-limit signals ONLY on capacity/rate-limit statuses. The bare
  // substrings otherwise false-match deterministic 400s like "context capacity exceeded" (input
  // too long) or "model has insufficient capacity for tools", wrongly escalating backoff on a
  // request that will never succeed until the client shrinks it. restrictToStatuses scopes them.
  { text: "capacity",   backoff: true, restrictToStatuses: [429, 502, 503, 504, 529] },
  { text: "overloaded", backoff: true, restrictToStatuses: [429, 502, 503, 504, 529] },

  // Content-moderation rejections (e.g. AgentRouter on Claude models) are deterministic: the
  // SAME prompt will be blocked again, so retrying/backoff is pointless. Some gateways return
  // these as 500, which would otherwise fall to the default 30s transient lock and freeze the
  // account for every subsequent request. Fail fast without locking.
  // Match on the human message text that parseUpstreamError surfaces (json.error.message),
  // e.g. "sensitive words detected (request id ...)" and "content-blocked (request id ...)".
  { text: "sensitive word",   shouldFallback: false, cooldownMs: 0 },
  { text: "content-blocked",  shouldFallback: false, cooldownMs: 0 },
  { text: "content blocked",  shouldFallback: false, cooldownMs: 0 },

  // --- Deterministic client errors: do NOT fall back and do NOT lock the account.
  // A 400/422 means the REQUEST itself is malformed or unsupported (bad param, oversized
  // context, tool-schema error). Retrying the identical request against every other account
  // cannot fix it and would burn + 30s-lock the whole pool on a single bad client request.
  { status: 400, shouldFallback: false, cooldownMs: 0 },
  { status: 422, shouldFallback: false, cooldownMs: 0 },

  // --- Status-based rules (fallback when text doesn't match) ---
  { status: 401, cooldownMs: COOLDOWN.long },
  { status: 402, cooldownMs: COOLDOWN.long },
  { status: 403, cooldownMs: COOLDOWN.long },
  { status: 404, cooldownMs: COOLDOWN.long },
  { status: 429, backoff: true },
];

// Backward compat: COOLDOWN_MS object (used by index.js re-export)
export const COOLDOWN_MS = {
  unauthorized: COOLDOWN.long,
  paymentRequired: COOLDOWN.long,
  notFound: COOLDOWN.long,
  transient: TRANSIENT_COOLDOWN_MS,
  requestNotAllowed: COOLDOWN.short,
};

/**
 * Substring signatures (lowercase) that identify an upstream *content-moderation*
 * rejection — deterministic for a given prompt+provider, so retrying the SAME
 * provider is pointless (the whole replayed history is re-scanned every turn).
 * See open-sse/config/errorConfig.js ERROR_RULES for the no-lock rules.
 */
export const MODERATION_SIGNATURES = ["content-blocked", "content blocked", "content_blocked", "sensitive word"];

/** True if an error message looks like a content-moderation rejection. */
export function isModerationError(errorText) {
  if (!errorText) return false;
  const s = String(errorText).toLowerCase();
  return MODERATION_SIGNATURES.some((sig) => s.includes(sig));
}

/**
 * When a provider moderates content (e.g. AgentRouter on Claude/GLM), re-run the
 * SAME request against a non-moderated provider serving the same model, keeping the
 * client-facing model name identical (transparent to the client; rescues sessions
 * whose replayed history already contains the flagged phrase).
 *
 * Keyed by source provider id → function mapping the upstream model to a
 * `provider/model` target, or null when no clean twin exists (then we surface the
 * original moderation error rather than silently mangle the request).
 */
export const MODERATION_FALLBACK = {
  agentrouter: (model) => {
    const m = String(model || "");
    if (/^claude/i.test(m)) return `claude/${m}`;   // ar/claude-opus-4-8 → claude/claude-opus-4-8
    if (/^glm/i.test(m)) return `glm/${m}`;          // ar/glm-5.2 → glm/glm-5.2
    return null;                                     // gpt-*/others: no clean twin → don't reroute
  },
};
