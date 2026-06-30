import { BaseExecutor } from "./base.js";
import { PROVIDERS } from "../config/providers.js";
import { injectReasoningContent } from "../utils/reasoningContentInjector.js";
import { ANTHROPIC_API_VERSION } from "../providers/shared.js";
import { getRequestContext, setRequestContext } from "./requestContext.js";

// Models that use /zen/go/v1/messages (Anthropic/Claude format + x-api-key auth)
const MESSAGES_FORMAT_MODELS = new Set([
  "minimax-m3",
  "minimax-m2.7",
  "minimax-m2.5",
  "qwen3.7-max",
  "qwen3.7-plus",
  "qwen3.6-plus",
]);

const BASE = "https://opencode.ai/zen/go/v1";

export class OpenCodeGoExecutor extends BaseExecutor {
  constructor() {
    super("opencode-go", PROVIDERS["opencode-go"]);
  }

  // The model is stored in the per-request context by base.execute (set before buildUrl), so read
  // it from there instead of caching on `this` (singleton would cross-contaminate concurrent reqs).
  // Record it here too so a buildUrl→buildHeaders call pair works even outside execute() (unit
  // tests, direct invocation) — setRequestContext is a no-op outside a request context, in which
  // case buildHeaders falls back to the model arg path below.
  buildUrl(model) {
    setRequestContext({ model });
    return MESSAGES_FORMAT_MODELS.has(model)
      ? `${BASE}/messages`
      : `${BASE}/chat/completions`;
  }

  buildHeaders(credentials, stream = true) {
    const key = credentials?.apiKey || credentials?.accessToken;
    const headers = { "Content-Type": "application/json" };

    // getRequestContext().model is set by base._execute before buildHeaders runs. Fall back to
    // undefined (→ Bearer auth) outside a request context (e.g. direct unit-test calls).
    const model = getRequestContext().model;
    if (MESSAGES_FORMAT_MODELS.has(model)) {
      headers["x-api-key"] = key;
      headers["anthropic-version"] = ANTHROPIC_API_VERSION;
    } else {
      headers["Authorization"] = `Bearer ${key}`;
    }

    if (stream) headers["Accept"] = "text/event-stream";
    return headers;
  }

  transformRequest(model, body) {
    return injectReasoningContent({ provider: this.provider, model, body });
  }
}
