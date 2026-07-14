// Generate backend/internal/httpapi/models_snapshot.json — the static catalog the
// native Go /v1/models handler embeds (go:embed). This mirrors the JS data that
// buildModelsList(["llm"]) reads from open-sse's provider registry, so Go can
// serve the common case without a Node round trip.
//
// GENERATED ARTIFACT — do not hand-edit models_snapshot.json. Regenerate after
// touching the provider registry or config/providerModels.js:
//
//     bun run gen:models-snapshot
//
// Scope note: /v1/models only ever requests kindFilter=["llm"], so search/fetch
// provider configs are intentionally NOT snapshotted (they surface only under
// webSearch/webFetch, handled by the proxied /v1/models/{kind} routes). We still
// emit every model's precomputed `kind` so Go filters identically without
// re-deriving MODEL_TYPE_TO_KIND.
//
// Faithfulness: the two alias maps and serviceKinds default replicate one-liners
// from open-sse. They are asserted below against PROVIDER_ID_TO_ALIAS (the live
// runtime value) so a drift in registry construction fails this script loudly.

import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(__dirname, "..");

const { PROVIDER_MODELS, PROVIDER_ID_TO_ALIAS } = await import(
  resolve(ROOT, "open-sse/config/providerModels.js").replace(/\\/g, "/")
);
const REGISTRY = (
  await import(resolve(ROOT, "open-sse/providers/registry/index.js").replace(/\\/g, "/"))
).default;
const { hasSpecializedExecutor } = await import(
  resolve(ROOT, "open-sse/executors/index.js").replace(/\\/g, "/")
);
const { getPricingForModel } = await import(
  resolve(ROOT, "open-sse/providers/pricing.js").replace(/\\/g, "/")
);
const { hasPotentialParamTransform } = await import(
  resolve(ROOT, "open-sse/translator/concerns/paramSupport.js").replace(/\\/g, "/")
);
const { needsReasoningContentTransform } = await import(
  resolve(ROOT, "open-sse/utils/reasoningContentInjector.js").replace(/\\/g, "/")
);

// ── kind derivation — byte-identical to modelKind() in
//    src/app/api/v1/models/route.js. Models without kind/type are LLM.
const MODEL_TYPE_TO_KIND = {
  image: "image",
  tts: "tts",
  embedding: "embedding",
  stt: "stt",
  imageToText: "imageToText",
};
const LLM_KIND = "llm";
function modelKind(model) {
  const k = model?.kind || model?.type;
  if (!k) return LLM_KIND;
  return MODEL_TYPE_TO_KIND[k] || LLM_KIND;
}

// ── native Go chat transport metadata ───────────────────────────────────────
// The first native /v1/chat/completions slice is deliberately conservative:
// inbound OpenAI Chat Completions → a single generic DefaultExecutor endpoint
// that already speaks OpenAI and uses simple Bearer auth. Anything with wire
// behavior Go has not ported yet (specialized executor, forced streaming,
// multi-format transport, request transforms, hooks/quirks, custom retries,
// no-auth/proxy-only behavior) stays on the Node fallback path.
function isSimpleBearerAuth(auth) {
  if (!auth) return true; // DefaultExecutor.resolveAuthDescriptor() → Bearer
  return auth.combined === true
    && auth.header === "Authorization"
    && auth.scheme === "bearer"
    && !auth.hooks?.length;
}

function nativeChatTransport(entry) {
  const t = entry.transport;
  if (!t?.baseUrl || hasSpecializedExecutor(entry.id)) return null;
  // DefaultExecutor overrides this provider's headers and chat.js owns its
  // content-moderation cross-provider reroute; neither is native yet.
  if (entry.id === "agentrouter") return null;
  if (t.baseUrl.includes("{")) return null;
  if ((t.format || "openai") !== "openai") return null;
  if (entry.transports?.length || t.baseUrls?.length) return null;
  if (t.forceStream || t.noAuth || entry.noAuth) return null;
  if (t.reasoningInject || t.urlSuffix || t.quirks && Object.keys(t.quirks).length) return null;
  if (t.retry && Object.keys(t.retry).length) return null;
  if (!isSimpleBearerAuth(t.auth)) return null;

  return {
    baseUrl: t.baseUrl,
    headers: t.headers || {},
    authHeader: "Authorization",
    authScheme: "bearer",
    timeoutMs: Number.isFinite(t.timeoutMs) ? t.timeoutMs : 60000,
  };
}

// ── alias maps, replicating open-sse formulas ───────────────────────────────
// idToAlias  = PROVIDER_ID_TO_ALIAS = OAUTH_ALIASES[id] || id
//              where OAUTH_ALIASES[id] = (alias && alias !== id) ? alias
// providerAlias = getProviderAlias(id) = AI_PROVIDERS[id].alias = uiAlias || alias
// serviceKinds  = AI_PROVIDERS[id].serviceKinds ?? ["llm"]  (top-level or media.*)
// AI_PROVIDERS in providers.js is the union of byCategory() over these five
// categories — NOT gated on transport. Media-only providers (transport:null,
// e.g. runwayml/stability-ai) still appear there with their real serviceKinds,
// so Node filters their non-llm models out of /v1/models. providerServiceKinds
// and providerAlias MUST cover the same set, or Go defaults the missing
// providers to ["llm"] and leaks their image/video models (the runwayml class).
const AI_CATEGORIES = new Set(["free", "freeTier", "oauth", "apikey", "webCookie"]);

const idToAlias = {};
const providerAlias = {};
const providerServiceKinds = {};
const providerLookup = {};
const nativeChatTransports = {};
const aiProviderIDs = new Set();

for (const r of REGISTRY) {
  const id = r.id;
  providerLookup[id] = id;
  if (r.alias) providerLookup[r.alias] = id;
  for (const alias of r.aliases || []) providerLookup[alias] = id;
  const nativeTransport = nativeChatTransport(r);
  if (nativeTransport) nativeChatTransports[id] = nativeTransport;
  // idToAlias mirrors PROVIDER_ID_TO_ALIAS, keyed on PROVIDERS (transport) only.
  if (r.transport) {
    idToAlias[id] = r.alias && r.alias !== id ? r.alias : id;
  }
  // providerAlias / providerServiceKinds mirror AI_PROVIDERS (category-gated).
  if (AI_CATEGORIES.has(r.category)) {
    aiProviderIDs.add(id);
    providerAlias[id] = r.uiAlias || r.alias || id;
    const sk = r.serviceKinds || (r.media && r.media.serviceKinds) || null;
    providerServiceKinds[id] = Array.isArray(sk) && sk.length ? sk : [LLM_KIND];
  }
}

// aliasToProviderId — reverse of idToAlias, replicating the exact JS one-liner in
// route.js (connections.length===0 branch). Object insertion order is registry
// order and last-write-wins on alias collisions; we build it the same way so Go
// need not reproduce JS map-ordering semantics (its own map iteration is random).
const aliasToProviderId = {};
for (const [id, alias] of Object.entries(idToAlias)) {
  aliasToProviderId[alias] = id;
}

// Drift guard: our reconstructed idToAlias must match the live runtime map for
// every provider it knows about. If open-sse changes how PROVIDER_ID_TO_ALIAS is
// built, this fails instead of silently shipping a stale snapshot.
const mismatches = [];
for (const [id, alias] of Object.entries(PROVIDER_ID_TO_ALIAS)) {
  if (idToAlias[id] !== alias) mismatches.push(`${id}: got ${idToAlias[id]}, want ${alias}`);
}
if (mismatches.length) {
  console.error("[gen-models-snapshot] idToAlias drift vs PROVIDER_ID_TO_ALIAS:");
  for (const m of mismatches.slice(0, 20)) console.error("  " + m);
  process.exit(1);
}

// Coverage guard for the runwayml media-only leak class. For every REAL registry
// provider in an AI category that owns models, Go must resolve its serviceKinds.
// Go resolves providerID = aliasToProviderId[modelsKey] ?? modelsKey (where
// modelsKey = alias||id, the PROVIDER_MODELS key), then reads
// providerServiceKinds[providerID], defaulting to ["llm"] when absent. If that
// resolution misses (e.g. a future media provider with alias≠id and
// transport:null, so it's absent from the transport-gated aliasToProviderId),
// Go would default it to ["llm"] and leak its image/video models while Node
// filters the whole provider out. Synthetic PROVIDER_MODELS keys (the *-tts-*
// tables) are intentionally excluded here: they are not registry providers, are
// absent from AI_PROVIDERS in Node too, so BOTH runtimes default them to ["llm"]
// at the provider level and then drop their tts-kind models per-model — no
// divergence.
const uncovered = [];
for (const r of REGISTRY) {
  if (r.models === undefined || !AI_CATEGORIES.has(r.category)) continue;
  const modelsKey = r.alias || r.id;
  const providerID = aliasToProviderId[modelsKey] ?? modelsKey;
  if (!(providerID in providerServiceKinds)) {
    uncovered.push(`${r.id} (modelsKey=${modelsKey} → ${providerID})`);
  }
}
if (uncovered.length) {
  console.error(
    "[gen-models-snapshot] AI-category providers whose serviceKinds Go cannot resolve\n" +
      "  (Go would default these to [\"llm\"] and leak non-llm models):"
  );
  for (const u of uncovered.slice(0, 30)) console.error("  " + u);
  process.exit(1);
}

// ── models, with precomputed kind ───────────────────────────────────────────
// providerModelsOrder preserves PROVIDER_MODELS insertion order (registry order,
// then appended TTS keys) so Go can iterate the static catalog in the same order
// Node does — a Go map loses key order, which would otherwise make the
// zero-connection fallback branch's output order nondeterministic.
const providerModels = {};
const providerModelsOrder = [];
let modelCount = 0;
for (const [alias, models] of Object.entries(PROVIDER_MODELS)) {
  providerModelsOrder.push(alias);
  const providerID = aliasToProviderId[alias] || alias;
  providerModels[alias] = models.map((m) => {
    modelCount++;
    const pricing = getPricingForModel(providerID, m.id) || getPricingForModel(alias, m.id);
    return {
      id: m.id,
      name: m.name || m.id,
      kind: modelKind(m),
      ...(m.upstreamModelId ? { upstreamId: m.upstreamModelId } : {}),
      ...(
        nativeChatTransports[providerID]
        && modelKind(m) === LLM_KIND
        && !m.targetFormat
        && !(m.strip || []).length
        && !hasPotentialParamTransform(providerID, m.id)
        && !needsReasoningContentTransform(providerID, m.id)
          ? { nativeChat: true }
          : {}
      ),
      ...(pricing ? { pricing } : {}),
    };
  });
}

const snapshot = {
  _generated: "bun run gen:models-snapshot — DO NOT EDIT BY HAND",
  _source: "open-sse provider registry + config/providerModels.js + executors/index.js",
  providerModels,
  providerModelsOrder,
  idToAlias,
  aliasToProviderId,
  providerAlias,
  providerServiceKinds,
  providerLookup,
  nativeChatTransports,
};

const outPath = resolve(ROOT, "backend/internal/httpapi/models_snapshot.json");
writeFileSync(outPath, JSON.stringify(snapshot, null, 2) + "\n");

console.log(
  `[gen-models-snapshot] wrote ${outPath}\n` +
    `  aliases=${Object.keys(providerModels).length} models=${modelCount} ` +
    `providers=${Object.keys(idToAlias).length}`
);
