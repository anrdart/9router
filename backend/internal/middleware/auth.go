package middleware

import (
	"context"
	"net/http"
	"strings"

	"9router/backend/internal/database"
)

// ctxKey for the DB reference injected into request contexts.
type dbCtxKey string

const dbKey dbCtxKey = "db"

// WithDB returns a shallow handler that injects the DB into each request context
// so downstream handlers can access it via DBFromContext.
func WithDB(db *database.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), dbKey, db)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// DBFromContext extracts the DB injected by WithDB, or nil.
func DBFromContext(ctx context.Context) *database.DB {
	if v, ok := ctx.Value(dbKey).(*database.DB); ok {
		return v
	}
	return nil
}

// loopbackHosts mirrors LOOPBACK_HOSTS in src/dashboardGuard.js.
var loopbackHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
}

// IsLocalRequest reports whether the request originates from a loopback host.
// This mirrors isLocalRequest() in dashboardGuard.js for the non-via-proxy case:
// the Host must be loopback, AND if an Origin header is present it too must be
// loopback (CSRF defence: a cross-origin browser request to a loopback host is
// treated as non-local). A non-loopback Host is never rescued by a loopback Origin.
func IsLocalRequest(r *http.Request) bool {
	host := hostName(r.Host)
	if !loopbackHosts[host] {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !loopbackHosts[hostName(origin)] {
			return false
		}
	}
	return true
}

// hostName extracts the bare hostname (no port, no brackets) from a Host header
// or URL string. Handles bracketed IPv6 literals like "[::1]:20128".
func hostName(s string) string {
	// Strip scheme if present (for Origin URLs).
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	// Strip path/query.
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	// Bracketed IPv6: [::1]:port or [::1]
	if strings.HasPrefix(s, "[") {
		if end := strings.IndexByte(s, ']'); end > 0 {
			return strings.ToLower(s[1:end])
		}
	}
	// Plain host:port — strip the last :port (but keep colons in bare IPv6).
	if strings.Count(s, ":") == 1 {
		if i := strings.LastIndexByte(s, ':'); i >= 0 {
			s = s[:i]
		}
	}
	return strings.ToLower(s)
}

// ExtractAPIKey mirrors extractApiKey() in dashboardGuard.js: Authorization
// Bearer, x-api-key, x-goog-api-key, or ?key= query param.
func ExtractAPIKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return k
	}
	if k := r.Header.Get("X-Goog-Api-Key"); k != "" {
		return k
	}
	return r.URL.Query().Get("key")
}

// CanAccessDashboard checks whether a request to a protected /api/* endpoint is
// allowed. This is the Phase-2 subset of dashboardGuard logic: local requests
// pass; otherwise a valid API key is required. Full JWT/OIDC/CLI-token parity is
// deferred to Phase 3 — until then, non-local browser sessions are proxied to
// Node (which enforces the complete auth rules).
func CanAccessDashboard(ctx context.Context, r *http.Request) bool {
	if IsLocalRequest(r) {
		return true
	}
	db := DBFromContext(ctx)
	if db == nil {
		return false
	}
	key := ExtractAPIKey(r)
	if key == "" {
		return false
	}
	ok, err := db.ValidateApiKey(ctx, key)
	return err == nil && ok
}

// RequireDashboardAuth gates a handler with CanAccessDashboard, returning 401 on
// failure. Non-local requests that lack a key fall through to the reverse proxy
// (handled at the mux level), so Node can still authenticate them via JWT.
func RequireDashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if CanAccessDashboard(r.Context(), r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
	})
}
