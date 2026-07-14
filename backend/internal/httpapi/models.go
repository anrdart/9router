package httpapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"9router/backend/internal/database"
	"9router/backend/internal/middleware"
)

// models_snapshot.json is the committed static catalog (provider models + alias
// maps) generated from open-sse by `bun run gen:models-snapshot`. It is embedded
// so the Go binary needs no Node round trip for the common /v1/models case.
//
//go:embed models_snapshot.json
var modelsSnapshotRaw []byte

// ── snapshot types ──────────────────────────────────────────────────────────

type modelPricing struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	Cached        float64 `json:"cached"`
	Reasoning     float64 `json:"reasoning"`
	CacheCreation float64 `json:"cache_creation"`
}

type snapModel struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Kind       string        `json:"kind"` // precomputed by the generator (modelKind())
	UpstreamID string        `json:"upstreamId,omitempty"`
	NativeChat bool          `json:"nativeChat,omitempty"`
	Pricing    *modelPricing `json:"pricing,omitempty"`
}

type nativeChatTransport struct {
	BaseURL    string            `json:"baseUrl"`
	Headers    map[string]string `json:"headers"`
	AuthHeader string            `json:"authHeader"`
	AuthScheme string            `json:"authScheme"`
	TimeoutMS  int               `json:"timeoutMs"`
}

type modelsSnapshot struct {
	ProviderModels       map[string][]snapModel         `json:"providerModels"`
	ProviderModelsOrder  []string                       `json:"providerModelsOrder"`
	IDToAlias            map[string]string              `json:"idToAlias"`
	AliasToProviderID    map[string]string              `json:"aliasToProviderId"`
	ProviderAlias        map[string]string              `json:"providerAlias"`
	ProviderServiceKinds map[string][]string            `json:"providerServiceKinds"`
	ProviderLookup       map[string]string              `json:"providerLookup"`
	NativeChatTransports map[string]nativeChatTransport `json:"nativeChatTransports"`
}

var snapshot modelsSnapshot

func init() {
	if err := json.Unmarshal(modelsSnapshotRaw, &snapshot); err != nil {
		// A malformed embedded artifact is a build/generation error, not a runtime
		// condition — fail fast at startup rather than serving a broken catalog.
		panic("httpapi: parse models_snapshot.json: " + err.Error())
	}
}

// ── constants ported 1:1 from src/app/api/v1/models/route.js ─────────────────

const llmKind = "llm"

// compatible-provider id prefixes (openAICompatiblePrefix / anthropicCompatiblePrefix
// are already declared in readonly.go).

// upstreamConnectionRE matches provider ids that are upstream/cross-instance
// connections (a UUID-ish hex suffix). Mirrors UPSTREAM_CONNECTION_RE.
var upstreamConnectionRE = regexp.MustCompile(`(?i)[-_][0-9a-f]{8,}$`)

// liveResolverProviders are the provider ids whose model catalog Node resolves
// over the network (LIVE_MODEL_RESOLVERS). When an active connection for one of
// these has no explicit enabledModels, we cannot reproduce the live list in Go,
// so the whole GET is reverse-proxied to Node for byte parity.
var liveResolverProviders = map[string]bool{
	"kiro": true, "qoder": true, "kimchi": true, "github": true, "clinepass": true,
}

// serviceKindCapabilities mirrors SERVICE_KIND_CAPABILITIES in
// open-sse/providers/capabilities.js — maps a custom model's service kind to the
// capability flags surfaced on the model entry. Returns nil for unmapped kinds.
var serviceKindCapabilities = map[string]map[string]any{
	"imageToText": {"vision": true},
	"image":       {"imageOutput": true},
	"stt":         {"audioInput": true},
	"tts":         {"audioOutput": true},
	"embedding":   {"tools": false},
}

// kind-inference regexes, ordered as in inferKindFromUnknownModelId (first match
// wins). Applied to unknown model ids to classify them.
var (
	reEmbed = regexp.MustCompile(`embed`)
	reTTS   = regexp.MustCompile(`tts|speech|audio|voice`)
	reImage = regexp.MustCompile(`image|imagen|dall-?e|flux|sdxl|sd-|stable-diffusion`)
)

func inferKindFromUnknownModelID(modelID string) string {
	lower := strings.ToLower(modelID)
	switch {
	case reEmbed.MatchString(lower):
		return "embedding"
	case reTTS.MatchString(lower):
		return "tts"
	case reImage.MatchString(lower):
		return "image"
	default:
		return llmKind
	}
}

// providerMatchesLLM reports whether a provider surfaces LLM models — the only
// kind /v1/models requests. Mirrors providerMatchesKinds(providerId, ["llm"]):
// serviceKinds default to ["llm"] when absent.
func providerMatchesLLM(providerID string) bool {
	kinds, ok := snapshot.ProviderServiceKinds[providerID]
	if !ok || len(kinds) == 0 {
		return true // default ["llm"]
	}
	for _, k := range kinds {
		if k == llmKind {
			return true
		}
	}
	return false
}

// ── response entry ───────────────────────────────────────────────────────────

type modelEntry struct {
	ID           string         `json:"id"`
	Object       string         `json:"object"`
	OwnedBy      string         `json:"owned_by"`
	Kind         string         `json:"kind,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

// ── parsed DB inputs ──────────────────────────────────────────────────────────

// activeConn is the minimum a connection contributes to the models list.
type activeConn struct {
	providerID    string
	prefix        string   // providerSpecificData.prefix (output alias override)
	enabledModels []string // providerSpecificData.enabledModels (explicit allowlist)
}

// customModel mirrors the customModels kv value shape.
type customModel struct {
	ProviderAlias string `json:"providerAlias"`
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
}

// modelData is everything the builder reads from the DB. Each field degrades to
// its zero value independently, matching the per-source try/catch in route.js.
type modelData struct {
	combos       []database.Combo
	customModels []customModel
	modelAliases []string            // values only (the full "alias/model" strings)
	disabled     map[string][]string // alias → disabled model ids
	// activeByProvider preserves first-seen (lowest-priority) connection per
	// provider, in a stable order for deterministic output.
	activeByProvider []activeConn
}

// loadModelData reads all model-related tables, tolerating per-source failure the
// same way buildModelsList() does (a failed source contributes nothing rather
// than failing the whole request). A nil db yields empty data → static branch.
func loadModelData(ctx context.Context, db *database.DB, logger *slog.Logger) modelData {
	d := modelData{disabled: map[string][]string{}}
	if db == nil {
		return d
	}

	if combos, err := db.ListCombos(ctx); err != nil {
		logger.Warn("models: could not fetch combos", slog.String("err", err.Error()))
	} else {
		d.combos = combos
	}

	if raw, err := db.KVScope(ctx, "customModels"); err != nil {
		logger.Warn("models: could not fetch custom models", slog.String("err", err.Error()))
	} else {
		for _, p := range raw {
			var cm customModel
			if json.Unmarshal(p.Value, &cm) == nil && cm.ID != "" {
				d.customModels = append(d.customModels, cm)
			}
		}
	}

	if raw, err := db.KVScope(ctx, "modelAliases"); err != nil {
		logger.Warn("models: could not fetch model aliases", slog.String("err", err.Error()))
	} else {
		for _, p := range raw {
			var s string
			if json.Unmarshal(p.Value, &s) == nil && s != "" {
				d.modelAliases = append(d.modelAliases, s)
			}
		}
	}

	if raw, err := db.KVScope(ctx, "disabledModels"); err != nil {
		logger.Warn("models: could not fetch disabled models", slog.String("err", err.Error()))
	} else {
		for _, p := range raw {
			var ids []string
			if json.Unmarshal(p.Value, &ids) == nil {
				d.disabled[p.Key] = ids
			}
		}
	}

	// Active connections → first (lowest-priority) per provider. ListProviderConnections
	// orders `priority ASC NULLS LAST, createdAt ASC`; Node sorts `(priority||999)`.
	// These agree for the normal priority range (create assigns max+1 ≥ 1, so 0 is
	// unreachable outside legacy imports) and NULL (both last). A priority-0 or
	// tied-priority row imported from legacy data could pick a different connection
	// than Node — a known, out-of-scope edge (that ORDER BY is shared Phase-2 code).
	conns, err := db.ListProviderConnections(ctx, "", true)
	if err != nil {
		logger.Warn("models: could not fetch providers, using static catalog", slog.String("err", err.Error()))
		return d
	}
	seen := map[string]bool{}
	for _, c := range conns {
		if seen[c.Provider] {
			continue
		}
		seen[c.Provider] = true
		d.activeByProvider = append(d.activeByProvider, parseActiveConn(c))
	}
	return d
}

// parseActiveConn extracts the fields the models list needs from a connection's
// opaque data blob (providerSpecificData.{prefix,enabledModels}).
func parseActiveConn(c database.ProviderConnection) activeConn {
	ac := activeConn{providerID: c.Provider}
	if c.Data == "" {
		return ac
	}
	var blob struct {
		PSD struct {
			Prefix        string   `json:"prefix"`
			EnabledModels []string `json:"enabledModels"`
		} `json:"providerSpecificData"`
	}
	if json.Unmarshal([]byte(c.Data), &blob) == nil {
		ac.prefix = strings.TrimSpace(blob.PSD.Prefix)
		for _, m := range blob.PSD.EnabledModels {
			if strings.TrimSpace(m) != "" {
				ac.enabledModels = append(ac.enabledModels, m)
			}
		}
	}
	return ac
}

// ── proxy decision ────────────────────────────────────────────────────────────

// needsNodeResolution reports whether any active connection would require a
// network call in Node (a live resolver, or compatible-provider /models
// discovery). In that case the whole GET is reverse-proxied for exact parity.
func (d modelData) needsNodeResolution() bool {
	for _, ac := range d.activeByProvider {
		if !providerMatchesLLM(ac.providerID) {
			continue
		}
		hasExplicit := len(ac.enabledModels) > 0

		// Live catalog resolver (kiro/qoder/kimchi/github/clinepass) with no
		// explicit allowlist → Node fetches the live list.
		if liveResolverProviders[ac.providerID] && !hasExplicit {
			return true
		}

		// OpenAI/Anthropic-compatible node with no explicit models and no static
		// catalog → Node discovers models from the upstream /models endpoint
		// (skipped for cross-instance upstream connections with a UUID suffix).
		isCompatible := strings.HasPrefix(ac.providerID, openAICompatiblePrefix) ||
			strings.HasPrefix(ac.providerID, anthropicCompatiblePrefix)
		if isCompatible && !hasExplicit {
			staticAlias := d.staticAlias(ac.providerID)
			if len(snapshot.ProviderModels[staticAlias]) == 0 && !upstreamConnectionRE.MatchString(ac.providerID) {
				return true
			}
		}
	}
	return false
}

// staticAlias = PROVIDER_ID_TO_ALIAS[id] || id.
func (d modelData) staticAlias(providerID string) string {
	if a, ok := snapshot.IDToAlias[providerID]; ok {
		return a
	}
	return providerID
}

// outputAlias = (prefix || getProviderAlias(id) || staticAlias).trim().
// getProviderAlias(id) = AI_PROVIDERS[id]?.alias || id, captured in the
// providerAlias snapshot; unknown ids fall through to staticAlias (== id).
func (d modelData) outputAlias(ac activeConn, staticAlias string) string {
	if ac.prefix != "" {
		return ac.prefix
	}
	if a, ok := snapshot.ProviderAlias[ac.providerID]; ok && a != "" {
		return a
	}
	return staticAlias
}

// ── native builder ────────────────────────────────────────────────────────────

func isDisabled(disabled map[string][]string, alias, modelID string) bool {
	for _, id := range disabled[alias] {
		if id == modelID {
			return true
		}
	}
	return false
}

// stripAlias removes a leading "<alias>/" from modelID for each candidate alias,
// mirroring the prefix-stripping in route.js. First match wins.
func stripAlias(modelID string, aliases ...string) string {
	for _, a := range aliases {
		if a == "" {
			continue
		}
		p := a + "/"
		if strings.HasPrefix(modelID, p) {
			return modelID[len(p):]
		}
	}
	return modelID
}

// buildModelsList reproduces buildModelsList(["llm"]) from route.js for the
// native (non-proxied) case. It returns OpenAI-format model entries.
func (d modelData) buildModelsList() []modelEntry {
	models := make([]modelEntry, 0, 64)

	// Combos first. Only kind ""/"llm" combos match the llm filter; web combos
	// (webSearch/webFetch) are excluded and never carry a kind here.
	for _, c := range d.combos {
		kind := c.Kind
		if kind == "" {
			kind = llmKind
		}
		if kind != llmKind {
			continue
		}
		models = append(models, modelEntry{ID: c.Name, Object: "model", OwnedBy: "combo"})
	}

	if len(d.activeByProvider) == 0 {
		models = d.appendStaticCatalog(models)
	} else {
		for _, ac := range d.activeByProvider {
			models = d.appendConnectionModels(models, ac)
		}
	}

	return dedupeByID(models)
}

// appendStaticCatalog is the connections.length===0 branch: the full static
// catalog filtered to LLM models, plus LLM custom models.
func (d modelData) appendStaticCatalog(models []modelEntry) []modelEntry {
	// Iterate in the snapshot's recorded order (== Node's PROVIDER_MODELS insertion
	// order) so output ordering is deterministic and matches Node.
	for _, alias := range snapshot.ProviderModelsOrder {
		provModels := snapshot.ProviderModels[alias]
		providerID := alias
		if id, ok := snapshot.AliasToProviderID[alias]; ok {
			providerID = id
		}
		if !providerMatchesLLM(providerID) {
			continue
		}
		for _, m := range provModels {
			if m.Kind != llmKind {
				continue
			}
			if isDisabled(d.disabled, alias, m.ID) {
				continue
			}
			models = append(models, modelEntry{ID: alias + "/" + m.ID, Object: "model", OwnedBy: alias})
		}
	}

	for _, cm := range d.customModels {
		if cm.ID == "" || (cm.Type != "" && cm.Type != llmKind) {
			continue
		}
		if cm.ProviderAlias == "" {
			continue
		}
		id := strings.TrimSpace(cm.ID)
		if id == "" {
			continue
		}
		models = append(models, modelEntry{ID: cm.ProviderAlias + "/" + id, Object: "model", OwnedBy: cm.ProviderAlias})
	}
	return models
}

// appendConnectionModels is the per-connection body of the else branch. The
// native path is only reached when no connection needs network resolution, so
// rawModelIds is always the explicit allowlist or the static catalog.
func (d modelData) appendConnectionModels(models []modelEntry, ac activeConn) []modelEntry {
	if !providerMatchesLLM(ac.providerID) {
		return models
	}

	staticAlias := d.staticAlias(ac.providerID)
	outputAlias := d.outputAlias(ac, staticAlias)
	provModels := snapshot.ProviderModels[staticAlias]

	// static kind lookup for filtering even when only ids are exposed
	staticKind := make(map[string]string, len(provModels))
	for _, m := range provModels {
		staticKind[m.ID] = m.Kind
	}

	// rawModelIds: explicit enabledModels (deduped) or the static catalog ids.
	var rawModelIDs []string
	if len(ac.enabledModels) > 0 {
		seen := map[string]bool{}
		for _, id := range ac.enabledModels {
			if !seen[id] {
				seen[id] = true
				rawModelIDs = append(rawModelIDs, id)
			}
		}
	} else {
		for _, m := range provModels {
			rawModelIDs = append(rawModelIDs, m.ID)
		}
	}

	// strip any alias prefix from raw ids
	modelIDs := make([]string, 0, len(rawModelIDs))
	for _, id := range rawModelIDs {
		s := stripAlias(id, outputAlias, staticAlias, ac.providerID)
		if strings.TrimSpace(s) != "" {
			modelIDs = append(modelIDs, s)
		}
	}

	// custom models attached to this provider (by any of its aliases), filtered
	// to the llm kind (imageToText allowed as a vision-capable chat model).
	customKind := map[string]string{}
	var customModelIDs []string
	for _, cm := range d.customModels {
		if cm.ID == "" {
			continue
		}
		kind := cm.Type
		if kind == "" {
			kind = llmKind
		}
		if kind != llmKind && kind != "imageToText" {
			continue
		}
		if cm.ProviderAlias != staticAlias && cm.ProviderAlias != outputAlias && cm.ProviderAlias != ac.providerID {
			continue
		}
		id := strings.TrimSpace(cm.ID)
		if id == "" {
			continue
		}
		customKind[id] = kind
		customModelIDs = append(customModelIDs, id)
	}

	// model aliases whose target belongs to this provider
	var aliasModelIDs []string
	for _, full := range d.modelAliases {
		if !strings.Contains(full, "/") {
			continue
		}
		if strings.HasPrefix(full, outputAlias+"/") ||
			strings.HasPrefix(full, staticAlias+"/") ||
			strings.HasPrefix(full, ac.providerID+"/") {
			s := stripAlias(full, outputAlias, staticAlias, ac.providerID)
			if strings.TrimSpace(s) != "" {
				aliasModelIDs = append(aliasModelIDs, s)
			}
		}
	}

	// merge (dedup) preserving order: static/explicit, then custom, then aliases
	merged := make([]string, 0, len(modelIDs)+len(customModelIDs)+len(aliasModelIDs))
	seen := map[string]bool{}
	for _, group := range [][]string{modelIDs, customModelIDs, aliasModelIDs} {
		for _, id := range group {
			if !seen[id] {
				seen[id] = true
				merged = append(merged, id)
			}
		}
	}

	for _, modelID := range merged {
		// kind: custom metadata → static → id heuristic (no live kind natively)
		kind := customKind[modelID]
		if kind == "" {
			kind = staticKind[modelID]
		}
		if kind == "" {
			kind = inferKindFromUnknownModelID(modelID)
		}
		allowAsLLM := kind == "imageToText" // llm always in the filter here
		if kind != llmKind && !allowAsLLM {
			continue
		}
		if isDisabled(d.disabled, outputAlias, modelID) || isDisabled(d.disabled, staticAlias, modelID) {
			continue
		}
		entry := modelEntry{ID: outputAlias + "/" + modelID, Object: "model", OwnedBy: outputAlias}
		// capabilities: only custom models carry a service-kind override natively
		// (liveCapabilities is never populated on the native path).
		if caps := serviceKindCapabilities[customKind[modelID]]; caps != nil {
			entry.Capabilities = caps
		}
		models = append(models, entry)
	}
	return models
}

// dedupeByID keeps the first entry per id, mirroring the final dedup in route.js.
func dedupeByID(models []modelEntry) []modelEntry {
	out := make([]modelEntry, 0, len(models))
	seen := map[string]bool{}
	for _, m := range models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

// ── handler ───────────────────────────────────────────────────────────────────

// NewModelsHandler returns the native GET /v1/models handler. It serves the
// common case (static catalog + DB combos/custom/aliases, minus disabled) from
// the embedded snapshot, and transparently reverse-proxies to Node when an
// active connection needs live/network model resolution. rp is the reverse proxy
// used for that fallback.
//
// This endpoint is intentionally public — the Node route has no auth gate, and
// the response exposes only model ids, never connection data or secrets.
func NewModelsHandler(rp http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		db := middleware.DBFromContext(r.Context())

		data := loadModelData(r.Context(), db, logger)

		if data.needsNodeResolution() {
			rp.ServeHTTP(w, r) // GET has no body; safe to re-serve upstream
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   data.buildModelsList(),
		})
	})
}
