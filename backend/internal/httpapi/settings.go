package httpapi

import (
	"encoding/json"
	"net/http"
	"os"

	"9router/backend/internal/middleware"
)

// secretSettingKeys must never be sent in a settings GET response — mirrors the
// PROTECTED_SETTING_KEYS / destructuring in src/app/api/settings/route.js.
var secretSettingKeys = map[string]bool{
	"password":          true,
	"oidcClientSecret":  true,
	"mitmSudoEncrypted": true,
}

// SettingsGET handles GET /api/settings.
//
// It returns the merged settings with secrets stripped, plus a few derived flags
// (hasPassword, oidcConfigured) — matching the response shape of
// src/app/api/settings/route.js so the dashboard sees no difference.
func SettingsGET(w http.ResponseWriter, r *http.Request) {
	db := middleware.DBFromContext(r.Context())
	if db == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}

	settings, err := db.GetSettings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Strip secrets and derive flags, matching the Node handler.
	hasPassword := false
	if _, ok := settings["password"].(string); ok && settings["password"].(string) != "" {
		hasPassword = true
	}
	oidcConfigured := false
	if issuer, _ := settings["oidcIssuerUrl"].(string); issuer != "" {
		if cid, _ := settings["oidcClientId"].(string); cid != "" {
			if secret, ok := settings["oidcClientSecret"].(string); ok && secret != "" {
				oidcConfigured = true
			}
		}
	}

	safe := map[string]any{}
	for k, v := range settings {
		if secretSettingKeys[k] {
			continue
		}
		safe[k] = v
	}
	safe["oidcConfigured"] = oidcConfigured
	safe["hasPassword"] = hasPassword
	// Env-derived feature flags, matching src/app/api/settings/route.js.
	safe["enableRequestLogs"] = os.Getenv("ENABLE_REQUEST_LOGS") == "true"
	safe["enableTranslator"] = os.Getenv("ENABLE_TRANSLATOR") == "true"

	writeJSON(w, http.StatusOK, safe)
}

// writeJSON encodes v as JSON with the given status and no-store caching.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a small JSON error object.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
