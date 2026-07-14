package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"9router/backend/internal/database"
	"9router/backend/internal/middleware"
)

// NewChatHandler serves the strict native OpenAI-compatible slice and sends any
// unsupported request to the existing Node handler with its original bytes
// intact. Native eligibility is fail-closed in resolveNativeChat.
func NewChatHandler(rp http.Handler, logger *slog.Logger) http.Handler {
	return newChatHandler(rp, logger, nil)
}

func newChatHandler(rp http.Handler, logger *slog.Logger, doer httpDoer) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))

		var body map[string]any
		if json.Unmarshal(raw, &body) != nil {
			writeOpenAIError(w, http.StatusBadRequest, "Invalid JSON body")
			return
		}
		model, _ := body["model"].(string)
		if model == "" {
			writeOpenAIError(w, http.StatusBadRequest, "Missing model")
			return
		}
		db := middleware.DBFromContext(r.Context())
		if db == nil {
			proxyOriginal(rp, w, r, raw)
			return
		}

		settings, err := db.GetSettings(r.Context())
		if err != nil {
			proxyOriginal(rp, w, r, raw)
			return
		}
		clientAPIKey := extractChatAPIKey(r)
		if required, _ := settings["requireApiKey"].(bool); required {
			if clientAPIKey == "" {
				writeOpenAIError(w, http.StatusUnauthorized, "Missing API key")
				return
			}
			valid, err := db.ValidateApiKey(r.Context(), clientAPIKey)
			if err != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "Internal server error")
				return
			}
			if !valid {
				writeOpenAIError(w, http.StatusUnauthorized, "Invalid API key")
				return
			}
		}

		nativeReq := nativeChatRequest{Model: model, Stream: streamRequested(body), Body: body, Raw: raw}
		resolution, native := resolveNativeChat(r.Context(), db, nativeReq)
		var comboResolutions []resolvedNativeChat
		if !native {
			comboResolutions, native = resolveNativeCombo(r.Context(), db, nativeReq, settings)
			if native {
				resolution = comboResolutions[0]
			}
		}
		if !native {
			proxyOriginal(rp, w, r, raw)
			return
		}
		start := time.Now()
		excluded := map[string]bool{}
		comboIndex := 0
		for {
			upstream, err := executeNativeUpstream(r.Context(), resolution, body, doer)
			if err != nil {
				if r.Context().Err() != nil {
					return // client disconnected; nothing useful can be written
				}
				logger.Warn("native chat upstream failed",
					slog.String("provider", resolution.Provider), slog.String("model", resolution.Model), slog.String("err", err.Error()))
				message := fmt.Sprintf("[502]: %s", err.Error())
				_, cooldown, _ := classifyFallback(http.StatusBadGateway, message, resolution.BackoffLevel)
				lockNativeFailure(r.Context(), db, resolution, http.StatusBadGateway, message, nil)
				saveNativeDetail(db, resolution, settings, body, start, "error", map[string]any{"error": err.Error(), "status": 502}, database.ChatUsage{})
				excluded[resolution.ConnectionID] = true
				if next, ok := nextNativeResolution(r.Context(), db, resolution, settings, excluded, comboResolutions, &comboIndex); ok {
					resolution = next
					continue
				}
				w.Header().Set("Retry-After", retryAfterSeconds(time.Now().Add(cooldown)))
				writeOpenAIError(w, http.StatusBadGateway, message)
				return
			}

			if upstream.Response.StatusCode < http.StatusOK || upstream.Response.StatusCode >= http.StatusMultipleChoices {
				status, message := parseUpstreamError(upstream.Response)
				_ = upstream.Response.Body.Close()
				fallback, cooldown, _ := classifyFallback(status, message, resolution.BackoffLevel)
				if fallback {
					lockNativeFailure(r.Context(), db, resolution, status, message, nil)
					saveNativeDetail(db, resolution, settings, body, start, "error", map[string]any{"error": message, "status": status}, database.ChatUsage{})
					excluded[resolution.ConnectionID] = true
					if next, ok := nextNativeResolution(r.Context(), db, resolution, settings, excluded, comboResolutions, &comboIndex); ok {
						resolution = next
						continue
					}
					w.Header().Set("Retry-After", retryAfterSeconds(time.Now().Add(cooldown)))
					message = fmt.Sprintf("[%s/%s] [%d]: %s (reset after %s)", resolution.Provider, resolution.Model, status, message, humanDuration(cooldown))
				} else {
					message = fmt.Sprintf("[%d]: %s", status, message)
					saveNativeDetail(db, resolution, settings, body, start, "error", map[string]any{"error": message, "status": status}, database.ChatUsage{})
				}
				writeOpenAIError(w, status, message)
				return
			}

			_ = db.ClearConnectionModelError(r.Context(), resolution.ConnectionID, resolution.Model, time.Now())

			if nativeReq.Stream {
				contentType := strings.ToLower(upstream.Response.Header.Get("Content-Type"))
				if contentType != "" && !strings.Contains(contentType, "text/event-stream") && !strings.Contains(contentType, "application/json") {
					_ = upstream.Response.Body.Close()
					writeOpenAIError(w, http.StatusBadGateway, "Upstream returned non-SSE response")
					return
				}
				usage, err := relayOpenAISSE(r.Context(), w, upstream.Response.Body, body)
				_ = upstream.Response.Body.Close()
				if err == nil && usage.Valid {
					tokens := database.ChatUsage{
						Prompt: usage.Prompt, Completion: usage.Completion, Total: usage.Prompt + usage.Completion,
						Cached: usage.Cached, Reasoning: usage.Reasoning, CacheCreation: usage.CacheCreation,
					}
					saveNativeUsage(db, resolution, clientAPIKey, tokens)
					saveNativeDetail(db, resolution, settings, body, start, "success", map[string]any{"type": "streaming"}, tokens)
				}
				return
			}

			responseRaw, err := io.ReadAll(io.LimitReader(upstream.Response.Body, 32<<20))
			_ = upstream.Response.Body.Close()
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "Invalid response from provider")
				return
			}
			normalized, usageObj, err := normalizeNonStreaming(responseRaw)
			if err != nil {
				writeOpenAIError(w, http.StatusBadGateway, "Invalid JSON response from "+resolution.Provider)
				return
			}
			tokens := usageFromMap(usageObj)
			if tokens.Prompt > 0 || tokens.Completion > 0 {
				// normalizeNonStreaming mutated client usage with +2000. Persist raw
				// provider usage, matching Node's storage/client separation.
				tokens.Prompt -= usageBufferTokens
				if tokens.Prompt < 0 {
					tokens.Prompt = 0
				}
				tokens.Total = tokens.Prompt + tokens.Completion
				saveNativeUsage(db, resolution, clientAPIKey, tokens)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(normalized)
			saveNativeDetail(db, resolution, settings, body, start, "success", map[string]any{"type": "json"}, tokens)
			return
		}
	})
}

func nextNativeResolution(ctx context.Context, db *database.DB, current resolvedNativeChat, settings map[string]any, excluded map[string]bool, combo []resolvedNativeChat, comboIndex *int) (resolvedNativeChat, bool) {
	if next, ok := selectNativeChatAccount(ctx, db, current, settings, excluded); ok {
		return next, true
	}
	if len(combo) == 0 {
		return resolvedNativeChat{}, false
	}
	for *comboIndex+1 < len(combo) {
		*comboIndex = *comboIndex + 1
		candidate := combo[*comboIndex]
		clear(excluded)
		if next, ok := selectNativeChatAccount(ctx, db, candidate, settings, excluded); ok {
			return next, true
		}
	}
	return resolvedNativeChat{}, false
}

func extractChatAPIKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.Header.Get("X-Api-Key")
}

func proxyOriginal(rp http.Handler, w http.ResponseWriter, r *http.Request, raw []byte) {
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	rp.ServeHTTP(w, r)
}

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	typeCode := map[int][2]string{
		400: {"invalid_request_error", "bad_request"}, 401: {"authentication_error", "invalid_api_key"},
		402: {"billing_error", "payment_required"}, 403: {"permission_error", "insufficient_quota"},
		404: {"invalid_request_error", "model_not_found"}, 406: {"invalid_request_error", "model_not_supported"},
		429: {"rate_limit_error", "rate_limit_exceeded"}, 500: {"server_error", "internal_server_error"},
		502: {"server_error", "bad_gateway"}, 503: {"server_error", "service_unavailable"}, 504: {"server_error", "gateway_timeout"},
	}[status]
	if typeCode[0] == "" {
		if status >= 500 {
			typeCode = [2]string{"server_error", "internal_server_error"}
		} else {
			typeCode = [2]string{"invalid_request_error", ""}
		}
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": typeCode[0], "code": typeCode[1]}})
}

func usageFromMap(m map[string]any) database.ChatUsage {
	if m == nil {
		return database.ChatUsage{}
	}
	p, _ := jsonNumber(m["prompt_tokens"])
	c, _ := jsonNumber(m["completion_tokens"])
	cached, _ := jsonNumber(m["cached_tokens"])
	if cached == 0 {
		if details, _ := m["prompt_tokens_details"].(map[string]any); details != nil {
			cached, _ = jsonNumber(details["cached_tokens"])
		}
	}
	if cached == 0 {
		cached, _ = jsonNumber(m["prompt_cache_hit_tokens"])
	}
	reasoning, _ := jsonNumber(m["reasoning_tokens"])
	if reasoning == 0 {
		if details, _ := m["completion_tokens_details"].(map[string]any); details != nil {
			reasoning, _ = jsonNumber(details["reasoning_tokens"])
		}
	}
	creation, _ := jsonNumber(m["cache_creation_input_tokens"])
	if creation == 0 {
		if details, _ := m["prompt_tokens_details"].(map[string]any); details != nil {
			creation, _ = jsonNumber(details["cache_creation_tokens"])
		}
	}
	return database.ChatUsage{
		Prompt: p, Completion: c, Total: p + c,
		Cached: cached, Reasoning: reasoning, CacheCreation: creation,
	}
}

func saveNativeUsage(db *database.DB, r resolvedNativeChat, apiKey string, tokens database.ChatUsage) {
	entry := database.ChatUsageEntry{
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Provider: r.Provider, Model: r.Model,
		ConnectionID: r.ConnectionID, APIKey: apiKey, Endpoint: "/v1/chat/completions", Status: "ok", Tokens: tokens,
		Cost: calculateNativeCost(tokens, r.Pricing),
	}
	_ = db.SaveChatUsage(contextWithoutCancel(), entry)
}

func saveNativeDetail(db *database.DB, r resolvedNativeChat, settings map[string]any, body map[string]any, started time.Time, status string, response map[string]any, tokens database.ChatUsage) {
	if enabled, ok := settings["enableObservability2"].(bool); ok && !enabled {
		return
	}
	if _, configured := settings["enableObservability2"]; !configured && strings.EqualFold(strings.TrimSpace(os.Getenv("OBSERVABILITY_ENABLED")), "false") {
		return
	}
	maxRecords := intSetting(settings, "observabilityMaxRecords", 200)
	maxBytes := intSetting(settings, "observabilityMaxJsonSize", 5) * 1024
	data := map[string]any{
		"provider": r.Provider, "model": r.Model, "connectionId": r.ConnectionID, "status": status,
		"latency": map[string]any{"ttft": time.Since(started).Milliseconds(), "total": time.Since(started).Milliseconds()},
		"tokens":  tokens, "request": truncateDetail(sanitizeNativeRequest(body), maxBytes), "providerRequest": nil, "providerResponse": nil, "response": truncateDetail(response, maxBytes),
	}
	_ = db.SaveChatRequestDetail(contextWithoutCancel(), "", r.Provider, r.Model, r.ConnectionID, status, data, maxRecords)
}

func intSetting(settings map[string]any, key string, fallback int) int {
	if n, ok := settings[key].(float64); ok && n > 0 {
		return int(n)
	}
	if n, ok := settings[key].(int); ok && n > 0 {
		return n
	}
	return fallback
}

func truncateDetail(value any, maxBytes int) any {
	raw, err := json.Marshal(value)
	if err != nil || maxBytes <= 0 || len(raw) <= maxBytes {
		return value
	}
	preview := string(raw)
	if len(preview) > 200 {
		preview = preview[:200]
	}
	return map[string]any{"_truncated": true, "_originalSize": len(raw), "_preview": preview}
}

func contextWithoutCancel() context.Context { return context.Background() }

func sanitizeNativeRequest(body map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"model", "stream", "temperature", "top_p", "max_tokens", "max_completion_tokens", "tools", "tool_choice", "response_format", "reasoning_effort"} {
		if v, ok := body[key]; ok {
			out[key] = v
		}
	}
	// Messages may contain user secrets; Node stores them only when observability
	// is enabled. The first native slice deliberately omits message content.
	if messages, ok := body["messages"].([]any); ok {
		out["messageCount"] = len(messages)
	}
	return out
}

func calculateNativeCost(t database.ChatUsage, p *modelPricing) float64 {
	if p == nil {
		return 0
	}
	nonCached := t.Prompt - t.Cached - t.CacheCreation
	if nonCached < 0 {
		nonCached = 0
	}
	cost := float64(nonCached)*p.Input/1_000_000 + float64(t.Completion)*p.Output/1_000_000
	cachedRate := p.Cached
	if cachedRate == 0 {
		cachedRate = p.Input
	}
	cost += float64(t.Cached) * cachedRate / 1_000_000
	reasoningRate := p.Reasoning
	if reasoningRate == 0 {
		reasoningRate = p.Output
	}
	cost += float64(t.Reasoning) * reasoningRate / 1_000_000
	creationRate := p.CacheCreation
	if creationRate == 0 {
		creationRate = p.Input
	}
	cost += float64(t.CacheCreation) * creationRate / 1_000_000
	return cost
}

func lockNativeFailure(ctx context.Context, db *database.DB, resolution resolvedNativeChat, status int, message string, w http.ResponseWriter) {
	fallback, cooldown, level := classifyFallback(status, message, resolution.BackoffLevel)
	if !fallback {
		return
	}
	until := time.Now().Add(cooldown)
	if err := db.UpdateConnectionChatState(ctx, resolution.ConnectionID, map[string]any{
		"modelLock_" + resolution.Model: until.UTC().Format(time.RFC3339Nano),
		"testStatus":                    "unavailable", "lastError": truncate(message, 100), "errorCode": status,
		"lastErrorAt": time.Now().UTC().Format(time.RFC3339Nano), "backoffLevel": level,
	}); err == nil && w != nil {
		w.Header().Set("Retry-After", retryAfterSeconds(until))
	}
}

func classifyFallback(status int, message string, backoffLevel int) (bool, time.Duration, int) {
	lower := strings.ToLower(message)
	for _, rule := range []struct {
		text     string
		cooldown time.Duration
	}{
		{"no credentials", 2 * time.Minute},
		{"request not allowed", 5 * time.Second},
		{"improperly formed request", 2 * time.Minute},
	} {
		if strings.Contains(lower, rule.text) {
			return true, rule.cooldown, backoffLevel
		}
	}
	for _, sig := range []string{"rate limit", "too many requests", "quota exceeded"} {
		if strings.Contains(lower, sig) {
			return rateBackoff(backoffLevel)
		}
	}
	for _, sig := range []string{"sensitive word", "content-blocked", "content blocked", "content_blocked"} {
		if strings.Contains(lower, sig) {
			return false, 0, backoffLevel
		}
	}
	if status == 400 || status == 422 {
		return false, 0, backoffLevel
	}
	if (strings.Contains(lower, "capacity") || strings.Contains(lower, "overloaded")) && (status == 429 || status == 502 || status == 503 || status == 504 || status == 529) {
		return rateBackoff(backoffLevel)
	}
	switch status {
	case 401, 402, 403, 404:
		return true, 2 * time.Minute, backoffLevel
	case 429:
		return rateBackoff(backoffLevel)
	default:
		return true, 30 * time.Second, backoffLevel
	}
}

func rateBackoff(level int) (bool, time.Duration, int) {
	level++
	if level > 15 {
		level = 15
	}
	exp := level - 1
	cooldown := 2 * time.Second * time.Duration(int64(1)<<exp)
	if cooldown > 5*time.Minute {
		cooldown = 5 * time.Minute
	}
	return true, cooldown, level
}

func humanDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	h, m, s := seconds/3600, (seconds%3600)/60, seconds%60
	parts := []string{}
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}
	return strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
