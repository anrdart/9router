package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"9router/backend/internal/database"
	"9router/backend/internal/middleware"
)

// Provider id prefixes for OpenAI/Anthropic-compatible custom nodes, matching
// src/shared/constants/providers.js. Compatible connections get their display
// name enriched from the providerNodes table.
const (
	openAICompatiblePrefix    = "openai-compatible-"
	anthropicCompatiblePrefix = "anthropic-compatible-"
)

// providerSecretFields are stripped from every /api/providers response. These
// exactly mirror the fields deleted in src/app/api/providers/route.js:69-76 so
// OAuth tokens and API keys stored in the opaque data blob are never returned.
var providerSecretFields = []string{"apiKey", "accessToken", "refreshToken", "idToken"}

// APIKeysGET handles GET /api/keys — returns {keys:[...]} ordered by createdAt,
// matching the Node response envelope the dashboard store expects.
func APIKeysGET(w http.ResponseWriter, r *http.Request) {
	db := middleware.DBFromContext(r.Context())
	if db == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	keys, err := db.ListAPIKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// ProvidersGET handles GET /api/providers. It returns {connections:[...]} with
// provider secrets stripped and compatible-provider names enriched, matching
// src/app/api/providers/route.js. The provider/isActive query params are an
// optional filtering superset (the dashboard fetches without params → all).
func ProvidersGET(w http.ResponseWriter, r *http.Request) {
	db := middleware.DBFromContext(r.Context())
	if db == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	q := r.URL.Query()
	provider := q.Get("provider")
	activeOnly := q.Get("isActive") == "true" || q.Get("active") == "true"

	conns, err := db.ListProviderConnections(r.Context(), provider, activeOnly)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	nodeNames := db.ProviderNodeNames(r.Context())

	out := make([]map[string]any, 0, len(conns))
	for _, c := range conns {
		j, err := c.ToJSON()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		redactProviderSecrets(j)
		j["name"] = enrichConnectionName(j, nodeNames)
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

// redactProviderSecrets removes credential fields from a serialized connection.
func redactProviderSecrets(j map[string]any) {
	for _, f := range providerSecretFields {
		delete(j, f)
	}
}

// enrichConnectionName reproduces the compatible-provider name fallback in
// route.js: name || nodeNameMap[provider] || providerSpecificData.nodeName ||
// provider. Non-compatible providers keep their stored name unchanged.
func enrichConnectionName(j map[string]any, nodeNames map[string]string) any {
	provider, _ := j["provider"].(string)
	name, _ := j["name"].(string)
	if !strings.HasPrefix(provider, openAICompatiblePrefix) && !strings.HasPrefix(provider, anthropicCompatiblePrefix) {
		return j["name"]
	}
	if name != "" {
		return name
	}
	if n := nodeNames[provider]; n != "" {
		return n
	}
	if psd, ok := j["providerSpecificData"].(map[string]any); ok {
		if nn, ok := psd["nodeName"].(string); ok && nn != "" {
			return nn
		}
	}
	return provider
}

// UsageLogsGET handles GET /api/usage/logs — returns a JSON array of pre-formatted
// log strings matching getRecentLogs() in usageRepo.js. The raw apiKey is never
// included (the Node query does not select it). Default 200 rows.
func UsageLogsGET(w http.ResponseWriter, r *http.Request) {
	db := middleware.DBFromContext(r.Context())
	if db == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	logs, err := db.RecentLogs(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

// UsageStatsGET handles GET /api/usage/stats — returns the persisted usage
// aggregation matching getUsageStats() in usageRepo.js. Live fields
// (activeRequests, pending, errorProvider) are emitted empty; the dashboard's
// SSE stream (proxied to Node) supplies them. Query param `period` defaults to
// "7d" and must be one of the valid periods.
func UsageStatsGET(w http.ResponseWriter, r *http.Request) {
	db := middleware.DBFromContext(r.Context())
	if db == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "7d"
	}
	if !database.ValidStatsPeriods[period] {
		writeError(w, http.StatusBadRequest, "Invalid period")
		return
	}
	stats, err := db.UsageStats(r.Context(), period, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
