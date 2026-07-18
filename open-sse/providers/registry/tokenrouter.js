// TokenRouter — OpenAI-compatible intelligent LLM routing gateway (tokenrouter.io).
// Single API key, POST /v1/chat/completions, Bearer auth. Drop-in OpenAI Chat
// Completions wire format, so it's served by DefaultExecutor at the default
// "openai" transport format — no custom executor, no translation hop.
//
// TokenRouter's signature is routing aliases in the `model` field: "auto:<strategy>"
// picks a provider/model automatically, and "<model>:<strategy>" pins a model but
// still routes across accounts. Confirmed strategies (docs.tokenrouter.io
// responses-api/modes + auto-routing, 2026-07-18): balance, cost, quality, latency.
// Concrete provider models pass through too (openai:gpt-4o, anthropic:claude-…,
// gpt-4o, gpt-4o:balance), so passthroughModels:true forwards any client id untouched
// and the static seed below only lists the four auto modes as UI hints.
//
// No GET /v1/models is documented (live probe 404s), so there's no validateUrl /
// modelsFetcher — the seed is static and passthrough covers everything else.
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
    website: "https://tokenrouter.io",
    notice: {
      apiKeyUrl: "https://tokenrouter.io/console/api-keys",
      text: "Intelligent routing gateway over OpenAI, Anthropic, Gemini, Mistral & more. Use auto:<strategy> (balance/cost/quality/latency) or any provider:model. Key from console (tr_…).",
    },
  },
  category: "apikey",
  authType: "apikey",
  authModes: ["apikey"],
  transport: {
    baseUrl: "https://api.tokenrouter.io/v1/chat/completions",
  },
  models: [
    { id: "auto:balance", name: "Auto — Balance (cost/quality)" },
    { id: "auto:quality", name: "Auto — Quality" },
    { id: "auto:cost", name: "Auto — Cost" },
    { id: "auto:latency", name: "Auto — Latency" },
  ],
  passthroughModels: true,
};
