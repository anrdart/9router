package httpapi

import (
	"net/http"
	"strconv"

	"9router/backend/internal/middleware"
)

// APIKeysGET handles GET /api/keys — returns all keys ordered by createdAt.
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
	writeJSON(w, http.StatusOK, keys)
}

// ProvidersGET handles GET /api/providers — returns connections, optionally
// filtered by provider/active query params (mirrors Node semantics).
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

	out := make([]map[string]any, 0, len(conns))
	for _, c := range conns {
		j, err := c.ToJSON()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// UsageLogsGET handles GET /api/usage/logs — returns recent usage rows.
// Query param `limit` (default 100, max 1000) controls the page size.
func UsageLogsGET(w http.ResponseWriter, r *http.Request) {
	db := middleware.DBFromContext(r.Context())
	if db == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := db.RecentUsage(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, u.ToJSON())
	}
	writeJSON(w, http.StatusOK, out)
}

// UsageStatsGET handles GET /api/usage/stats — returns a small summary object.
func UsageStatsGET(w http.ResponseWriter, r *http.Request) {
	db := middleware.DBFromContext(r.Context())
	if db == nil {
		writeError(w, http.StatusInternalServerError, "database unavailable")
		return
	}
	total, oldest, newest, err := db.UsageCount(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{
		"total": total,
	}
	if oldest.Valid {
		resp["oldest"] = oldest.String
	}
	if newest.Valid {
		resp["newest"] = newest.String
	}
	writeJSON(w, http.StatusOK, resp)
}
