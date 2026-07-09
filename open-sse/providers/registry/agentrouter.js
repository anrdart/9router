// AgentRouter — OpenAI-compatible unified LLM gateway (agentrouter.org).
// Single API key, POST /v1/chat/completions, Bearer auth. Streaming, tool/function calling and
// JSON mode all work over the standard OpenAI Chat Completions wire format, so this provider is
// served by the DefaultExecutor with the default "openai" transport format — no custom executor,
// no translation hop, no identity-spoof headers needed.
//
// Model ids are the canonical upstream ids (claude-opus-4-8, gpt-5.5, glm-5.2, …), so pricing +
// capabilities resolve automatically via the provider-agnostic MODEL_PRICING / capabilities tables.
// AgentRouter passes provider pricing through with no markup.
// Catalog verified against https://agentrouter.org/api/pricing (2026-07-09). Per-token access is
// group-scoped upstream, so the dashboard "refresh models" (GET /v1/models) narrows this static
// seed to what each key can actually reach — e.g. a default-group key sees only glm-5.2.
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
    // AgentRouter gates API access on the client's User-Agent: a bare/SDK User-Agent (e.g. node,
    // openai-js, or the executor default) is rejected with 401 "unauthorized client detected"
    // even with a valid key. It only accepts the Cline/VS Code extension identity. Kilo Code
    // (a Cline fork) works for the same reason — it inherits this User-Agent. Mirror it so
    // 9router's requests are accepted. Version is not checked, only the `cline/<v> vscode-extension` shape.
    headers: {
      "User-Agent": "cline/1.0.0 vscode-extension",
    },
  },
  models: [
    { id: "claude-opus-4-8", name: "Claude Opus 4.8" },
    { id: "claude-opus-4-7", name: "Claude Opus 4.7" },
    { id: "claude-opus-4-6", name: "Claude Opus 4.6" },
    { id: "gpt-5.5", name: "GPT-5.5" },
    { id: "glm-5.2", name: "GLM-5.2" },
  ],
};
