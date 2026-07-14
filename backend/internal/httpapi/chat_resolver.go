package httpapi

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"9router/backend/internal/database"
)

// nativeChatRequest is the minimum OpenAI Chat Completions request shape needed
// to decide whether Go can handle a request without losing behavior. Body keeps
// the original object so unknown OpenAI-compatible fields pass through intact.
type nativeChatRequest struct {
	Model  string         `json:"model"`
	Stream bool           `json:"stream"`
	Body   map[string]any `json:"-"`
	Raw    []byte         `json:"-"`
}

// chatConnectionData mirrors only connection data consumed by the first native
// OpenAI-compatible slice. Credentials stay internal and are never serialized.
type chatConnectionData struct {
	APIKey              string `json:"apiKey"`
	AccessToken         string `json:"accessToken"`
	RefreshToken        string `json:"refreshToken"`
	ExpiresAt           string `json:"expiresAt"`
	TestStatus          string `json:"testStatus"`
	LastError           string `json:"lastError"`
	ErrorCode           int    `json:"errorCode"`
	BackoffLevel        int    `json:"backoffLevel"`
	LastUsedAt          string `json:"lastUsedAt"`
	ConsecutiveUseCount int    `json:"consecutiveUseCount"`
	PSD                 struct {
		ConnectionProxyEnabled bool   `json:"connectionProxyEnabled"`
		ConnectionProxyURL     string `json:"connectionProxyUrl"`
		ConnectionNoProxy      string `json:"connectionNoProxy"`
		VercelRelayURL         string `json:"vercelRelayUrl"`
	} `json:"providerSpecificData"`
}

type resolvedNativeChat struct {
	Provider      string
	Model         string
	UpstreamModel string
	ConnectionID  string
	Credential    string
	Transport     nativeChatTransport
	Pricing       *modelPricing
	BackoffLevel  int
}

// resolveNativeChat returns (resolution, true) only when the request can be
// served by Go with the exact behavior currently implemented. false means
// transparently proxy the untouched request to Node — never degrade features.
func resolveNativeChat(ctx context.Context, db *database.DB, req nativeChatRequest) (resolvedNativeChat, bool) {
	base, settings, ok := resolveNativeChatBase(ctx, db, req)
	if !ok {
		return resolvedNativeChat{}, false
	}
	return selectNativeChatAccount(ctx, db, base, settings, nil)
}

func resolveNativeChatBase(ctx context.Context, db *database.DB, req nativeChatRequest) (resolvedNativeChat, map[string]any, bool) {
	if db == nil || req.Model == "" || !strings.Contains(req.Model, "/") || !nativeOpenAIChatBody(req.Body) {
		return resolvedNativeChat{}, nil, false
	}
	// Node defaults omitted stream to SSE. Keep the first native slice explicit so
	// transport choice and wire framing cannot drift.
	if _, ok := req.Body["stream"].(bool); !ok {
		return resolvedNativeChat{}, nil, false
	}

	firstSlash := strings.IndexByte(req.Model, '/')
	providerToken := req.Model[:firstSlash]
	model := stripContextSuffix(req.Model[firstSlash+1:])
	if providerToken == "" || model == "" {
		return resolvedNativeChat{}, nil, false
	}
	provider := snapshot.ProviderLookup[providerToken]
	if provider == "" {
		return resolvedNativeChat{}, nil, false // custom provider nodes stay proxied
	}
	transport, ok := snapshot.NativeChatTransports[provider]
	if !ok || transport.BaseURL == "" {
		return resolvedNativeChat{}, nil, false
	}

	staticAlias := snapshot.IDToAlias[provider]
	if staticAlias == "" {
		staticAlias = provider
	}
	upstreamModel, pricing, safe := nativeSafeModel(staticAlias, model)
	if !safe {
		return resolvedNativeChat{}, nil, false
	}
	pricing = resolveNativePricing(ctx, db, provider, model, pricing)

	settings, err := db.GetSettings(ctx)
	if err != nil || nativeChatNeedsNodeSettings(settings, provider, req.Body) {
		return resolvedNativeChat{}, nil, false
	}
	return resolvedNativeChat{
		Provider: provider, Model: model, UpstreamModel: upstreamModel,
		Transport: transport, Pricing: pricing,
	}, settings, true
}

var accountSelectionLocks sync.Map

func providerSelectionLock(provider string) *sync.Mutex {
	lock, _ := accountSelectionLocks.LoadOrStore(provider, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func selectNativeChatAccount(ctx context.Context, db *database.DB, base resolvedNativeChat, settings map[string]any, exclude map[string]bool) (resolvedNativeChat, bool) {
	lock := providerSelectionLock(base.Provider)
	lock.Lock()
	defer lock.Unlock()

	conns, err := db.ListProviderConnections(ctx, base.Provider, true)
	if err != nil || len(conns) == 0 {
		return resolvedNativeChat{}, false
	}

	type candidate struct {
		conn database.ProviderConnection
		data chatConnectionData
	}
	available := make([]candidate, 0, len(conns))
	now := time.Now()
	for _, conn := range conns {
		if exclude[conn.ID] {
			continue
		}
		var data chatConnectionData
		if conn.Data == "" || json.Unmarshal([]byte(conn.Data), &data) != nil || !nativeConnectionSafe(data) {
			// Account selection must not silently drop a connection Node would use.
			// Proxy the whole request instead.
			return resolvedNativeChat{}, false
		}
		if modelLockActive(dataFromJSON(conn.Data), base.Model, now) {
			continue
		}
		available = append(available, candidate{conn: conn, data: data})
	}
	if len(available) == 0 {
		return resolvedNativeChat{}, false
	}

	selected := available[0]
	if nativeFallbackStrategy(settings, base.Provider) == "round-robin" {
		sticky := nativeStickyLimit(settings, base.Provider)
		sort.SliceStable(available, func(i, j int) bool {
			left, lok := parseSelectionTime(available[i].data.LastUsedAt)
			right, rok := parseSelectionTime(available[j].data.LastUsedAt)
			if lok != rok {
				return lok // used accounts first
			}
			if lok && !left.Equal(right) {
				return left.After(right)
			}
			return connectionPriority(available[i].conn) < connectionPriority(available[j].conn)
		})
		current := available[0]
		if current.data.LastUsedAt != "" && current.data.ConsecutiveUseCount < sticky {
			selected = current
			selected.data.ConsecutiveUseCount++
		} else {
			sort.SliceStable(available, func(i, j int) bool {
				left, lok := parseSelectionTime(available[i].data.LastUsedAt)
				right, rok := parseSelectionTime(available[j].data.LastUsedAt)
				if lok != rok {
					return !lok // never-used accounts first
				}
				if lok && !left.Equal(right) {
					return left.Before(right)
				}
				return connectionPriority(available[i].conn) < connectionPriority(available[j].conn)
			})
			selected = available[0]
			selected.data.ConsecutiveUseCount = 1
		}
		selected.data.LastUsedAt = now.UTC().Format(time.RFC3339Nano)
		if db.UpdateConnectionSelectionState(ctx, selected.conn.ID, selected.data.LastUsedAt, selected.data.ConsecutiveUseCount) != nil {
			return resolvedNativeChat{}, false
		}
	}

	credential := selected.data.APIKey
	if credential == "" {
		credential = selected.data.AccessToken
	}
	if credential == "" {
		return resolvedNativeChat{}, false
	}
	base.ConnectionID = selected.conn.ID
	base.Credential = credential
	base.BackoffLevel = selected.data.BackoffLevel
	return base, true
}

func nativeConnectionSafe(data chatConnectionData) bool {
	return data.RefreshToken == "" && data.ExpiresAt == "" &&
		!data.PSD.ConnectionProxyEnabled && data.PSD.ConnectionProxyURL == "" &&
		data.PSD.ConnectionNoProxy == "" && data.PSD.VercelRelayURL == ""
}

func connectionPriority(conn database.ProviderConnection) int64 {
	if conn.Priority.Valid {
		return conn.Priority.Int64
	}
	return 999
}

func parseSelectionTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil
}

func nativeFallbackStrategy(settings map[string]any, provider string) string {
	if strategies, ok := settings["providerStrategies"].(map[string]any); ok {
		if cfg, ok := strategies[provider].(map[string]any); ok {
			if strategy, _ := cfg["fallbackStrategy"].(string); strategy != "" {
				return strategy
			}
		}
	}
	if strategy, _ := settings["fallbackStrategy"].(string); strategy != "" {
		return strategy
	}
	return "fill-first"
}

func nativeStickyLimit(settings map[string]any, provider string) int {
	if strategies, ok := settings["providerStrategies"].(map[string]any); ok {
		if cfg, ok := strategies[provider].(map[string]any); ok {
			if n := numberSetting(cfg["stickyRoundRobinLimit"]); n > 0 {
				return n
			}
		}
	}
	if n := numberSetting(settings["stickyRoundRobinLimit"]); n > 0 {
		return n
	}
	return 3
}

func numberSetting(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

func nativeOpenAIChatBody(body map[string]any) bool {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 || body["input"] != nil || body["contents"] != nil || body["request"] != nil || body["system"] != nil {
		return false
	}
	for _, key := range []string{
		"thinking", "reasoning", "reasoning_effort", "thinkingConfig",
		"enable_thinking", "thinking_budget", "output_config", "client_metadata",
	} {
		if _, present := body[key]; present {
			return false
		}
	}
	// These Node normalization paths mutate bodies. Proxy instead of reproducing
	// the translator in the initial native slice.
	if tools, present := body["tools"]; present {
		if list, ok := tools.([]any); !ok || len(list) == 0 {
			return false
		}
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		role, _ := msg["role"].(string)
		if role != "system" && role != "user" && role != "assistant" {
			return false
		}
		if _, ok := msg["content"].(string); !ok {
			return false
		}
		if msg["tool_calls"] != nil || msg["tool_call_id"] != nil {
			return false
		}
	}
	return true
}

func stripContextSuffix(model string) string {
	if i := strings.LastIndexByte(model, '['); i >= 0 && strings.HasSuffix(model, "]") {
		return model[:i]
	}
	return model
}

func nativeSafeModel(alias, model string) (string, *modelPricing, bool) {
	for _, m := range snapshot.ProviderModels[alias] {
		if m.ID != model || m.Kind != llmKind || !m.NativeChat {
			continue
		}
		if m.UpstreamID != "" {
			return m.UpstreamID, m.Pricing, true
		}
		return m.ID, m.Pricing, true
	}
	return "", nil, false
}

func resolveNativePricing(ctx context.Context, db *database.DB, provider, model string, fallback *modelPricing) *modelPricing {
	pairs, err := db.KVScope(ctx, "pricing")
	if err != nil {
		return fallback
	}
	for _, pair := range pairs {
		if pair.Key != provider {
			continue
		}
		var models map[string]modelPricing
		if json.Unmarshal(pair.Value, &models) != nil {
			return fallback
		}
		if p, ok := models[model]; ok {
			return &p
		}
		return fallback
	}
	return fallback
}

func nativeChatNeedsNodeSettings(settings map[string]any, provider string, body map[string]any) bool {
	// These features mutate every eligible request (or depend on JS-only modules),
	// so preserve them by proxying while enabled.
	for _, key := range []string{"headroomEnabled", "cavemanEnabled", "ponytailEnabled", "pxpipeEnabled"} {
		if enabled, _ := settings[key].(bool); enabled {
			return true
		}
	}
	// RTK is on by default but only changes tool-result messages. Ordinary chat
	// requests remain byte-equivalent and can use the native path.
	if enabled, _ := settings["rtkEnabled"].(bool); enabled && hasToolResult(body) {
		return true
	}
	if enabled, _ := settings["ccFilterNaming"].(bool); enabled && looksLikeNamingRequest(body) {
		return true
	}
	if all, ok := settings["providerThinking"].(map[string]any); ok {
		if cfg, ok := all[provider].(map[string]any); ok {
			if mode, _ := cfg["mode"].(string); mode != "" && mode != "auto" {
				return true
			}
		}
	}
	return false
}

func hasToolResult(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if role, _ := msg["role"].(string); role == "tool" {
			return true
		}
		blocks, _ := msg["content"].([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			if typ, _ := block["type"].(string); typ == "tool_result" {
				return true
			}
		}
	}
	return false
}

func looksLikeNamingRequest(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		content, _ := msg["content"].(string)
		if strings.Contains(content, "Please write a 5-10 word title for the following conversation:") {
			return true
		}
	}
	return false
}

func dataFromJSON(raw string) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func modelLockActive(data map[string]any, model string, now time.Time) bool {
	for _, key := range []string{"modelLock_" + model, "modelLock___all"} {
		s, _ := data[key].(string)
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(now) {
			return true
		}
	}
	return false
}
