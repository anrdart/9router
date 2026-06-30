// AgentRouter — OpenAI-compatible unified LLM gateway (agentrouter.org).
// Single API key, POST /v1/chat/completions, Bearer auth. Streaming, tool/function calling and
// JSON mode all work over the standard OpenAI Chat Completions wire format, so this provider is
// served by the DefaultExecutor with the default "openai" transport format — no custom executor,
// no translation hop, no identity-spoof headers needed.
//
// Model ids are the canonical upstream ids (claude-sonnet-4-5-20250929, gpt-4o, deepseek-r1, …),
// so pricing + capabilities resolve automatically via the provider-agnostic MODEL_PRICING /
// capabilities tables. AgentRouter passes provider pricing through with no markup.
export default {
  id: "agentrouter",
  priority: 105,
  alias: "agentrouter",
  aliases: ["ar"],
  uiAlias: "ar",
  display: {
    name: "AgentRouter",
    icon: "bolt",
    color: "#8B5CF6",
    textIcon: "AR",
    website: "https://agentrouter.org",
    notice: {
      apiKeyUrl: "https://agentrouter.org/console/token",
      text: "OpenAI-compatible gateway aggregating Claude, GPT, Gemini, GLM, DeepSeek and more. Get a key from the console.",
    },
  },
  category: "apikey",
  authType: "apikey",
  authModes: ["apikey"],
  transport: {
    baseUrl: "https://agentrouter.org/v1/chat/completions",
    validateUrl: "https://agentrouter.org/v1/models",
    // Models like deepseek-r1 / glm-4.6 expose reasoning over the OpenAI reasoning_content shape.
    thinkingFormat: "openai",
  },
  models: [
    { id: "claude-sonnet-4-5-20250929", name: "Claude Sonnet 4.5" },
    { id: "claude-opus-4-5-20250929", name: "Claude Opus 4.5" },
    { id: "claude-haiku-4-5-20251001", name: "Claude Haiku 4.5" },
    { id: "gpt-4o", name: "GPT-4o" },
    { id: "gpt-4o-mini", name: "GPT-4o Mini" },
    { id: "glm-4.6", name: "GLM-4.6" },
    { id: "glm-4.6v", name: "GLM-4.6V (Vision)" },
    { id: "glm-4.5-air", name: "GLM-4.5 Air" },
    { id: "deepseek-r1", name: "DeepSeek R1" },
    { id: "qwen3-coder-480b", name: "Qwen3 Coder 480B" },
    { id: "gemini-2-0-pro", name: "Gemini 2.0 Pro" },
  ],
};
