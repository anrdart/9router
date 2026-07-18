// TokenRouter — PBD TokenRouter by PaleBlueDot AI (tokenrouter.com), an
// OpenAI-compatible unified gateway aggregating 300+ models (OpenAI, Anthropic,
// Google, xAI, DeepSeek, Qwen, GLM, Moonshot, MiniMax, Mistral, …).
//
// One-api/new-api style gateway (same family as AgentRouter): single API key from
// the console, POST /v1/chat/completions, Bearer auth, served by DefaultExecutor
// at the default "openai" transport format — no custom executor, no translation.
//
// Model ids are the canonical vendor-prefixed upstream ids (anthropic/claude-opus-4.8,
// openai/gpt-5.2, deepseek/deepseek-v4-pro, …). Per-key access is group-scoped
// upstream, so validateUrl (GET /v1/models) narrows this static flagship seed to
// what each key can actually reach, and passthroughModels forwards any of the 300+
// ids untouched. Catalog verified against https://api.tokenrouter.com/api/pricing
// (2026-07-18).
export default {
  id: "tokenrouter",
  priority: 106,
  alias: "tokenrouter",
  aliases: ["tr"],
  uiAlias: "tr",
  display: {
    name: "TokenRouter",
    icon: "route",
    color: "#10B981",
    textIcon: "TR",
    website: "https://tokenrouter.com",
    notice: {
      apiKeyUrl: "https://www.tokenrouter.com/console/token",
      text: "PBD TokenRouter (PaleBlueDot AI): OpenAI-compatible gateway aggregating 300+ models — Claude, GPT, Gemini, Grok, DeepSeek, Qwen, GLM and more. Get a key from the console.",
    },
  },
  category: "apikey",
  authType: "apikey",
  authModes: ["apikey"],
  transport: {
    baseUrl: "https://api.tokenrouter.com/v1/chat/completions",
    validateUrl: "https://api.tokenrouter.com/v1/models",
    // NOTE: do NOT pin a provider-wide thinkingFormat here. This gateway fronts many
    // vendors (Claude, GPT, DeepSeek, GLM/Z.ai, Qwen…) whose reasoning wire formats
    // differ. resolveFormat() prefers a provider-wide thinkingFormat over per-model
    // capabilities, so pinning "openai" would force every model down the OpenAI
    // reasoning_effort path — leaking client efforts like "xhigh" to a GLM/sglang
    // upstream that only accepts none/low/medium/high/max (HTTP 400). Let each model's
    // own capabilities pick its native format (glm→zai, deepseek→deepseek, …).
  },
  models: [
    { id: "anthropic/claude-opus-4.8", name: "Claude Opus 4.8" },
    { id: "anthropic/claude-sonnet-5", name: "Claude Sonnet 5" },
    { id: "openai/gpt-5.2", name: "GPT-5.2" },
    { id: "google/gemini-3.1-flash-lite-image", name: "Gemini 3.1 Flash Lite" },
    { id: "x-ai/grok-4.5", name: "Grok 4.5" },
    { id: "deepseek/deepseek-v4-pro", name: "DeepSeek V4 Pro" },
    { id: "qwen/qwen3.7-max", name: "Qwen3.7 Max" },
    { id: "z-ai/glm-5.2", name: "GLM-5.2" },
    { id: "moonshotai/kimi-k3", name: "Kimi K3" },
    { id: "minimax/minimax-m2.7", name: "MiniMax M2.7" },
  ],
  passthroughModels: true,
};
