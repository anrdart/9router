package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"9router/backend/internal/auth"
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

// authCookie is the dashboard JWT cookie name (matches dashboardSession.js).
const authCookie = "auth_token"

// cliTokenHeader carries the CLI machine token (matches dashboardGuard.js).
const cliTokenHeader = "X-9R-Cli-Token"

// forcePasswordChangeAllowed mirrors FORCE_PASSWORD_CHANGE_ALLOWED in
// dashboardGuard.js. A session flagged force_password_change may reach ONLY these
// paths (so a leaked/known default password cannot unlock stored credentials).
// Of the native Go routes only /api/settings is in this set; the others 403.
var forcePasswordChangeAllowed = map[string]bool{
	"/api/settings":    true,
	"/api/auth/status": true,
	"/api/auth/logout": true,
}

// Guard enforces dashboard auth on protected native /api/* routes. It reads the
// SAME secrets Node writes under dataDir, so a browser session (auth_token JWT)
// or CLI token minted by Node is accepted unchanged — no reverse-proxy round trip.
//
// This intentionally does NOT grant access on host locality or on an inbound LLM
// API key: dashboardGuard.js gates /api/* on dashboard JWT / CLI token /
// requireLogin===false only. Binding loopback-by-default (see config.Host) is the
// separate network control.
type Guard struct {
	dataDir string
	now     func() time.Time // injectable for tests
}

// NewGuard builds a Guard reading Node's secrets from dataDir.
func NewGuard(dataDir string) *Guard {
	return &Guard{dataDir: dataDir, now: time.Now}
}

// jwtSession verifies the auth_token cookie and returns its claims, or nil if
// absent/invalid/expired. The JWT secret is read per call (a tiny file) so Go
// picks it up even when Node creates it lazily after Go has started.
func (g *Guard) jwtSession(r *http.Request) *auth.Claims {
	c, err := r.Cookie(authCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	secret := auth.LoadJWTSecret(g.dataDir)
	claims, err := auth.VerifyJWT(c.Value, secret, g.now())
	if err != nil {
		return nil
	}
	return claims
}

// allowed reports whether a GET to a protected native /api/* route is permitted,
// and if not, the status to return (403 for a force-password-change lockout,
// otherwise 401). Mirrors the /api/* branch of dashboardGuard.proxy().
func (g *Guard) allowed(r *http.Request) (ok bool, status int) {
	session := g.jwtSession(r)

	// A force-password-change session is locked to password-setting routes only.
	if session != nil && session.ForcePasswordChange {
		if forcePasswordChangeAllowed[r.URL.Path] {
			return true, 0
		}
		return false, http.StatusForbidden
	}

	// CLI machine token.
	if auth.ValidCLIToken(r.Header.Get(cliTokenHeader), g.dataDir) {
		return true, 0
	}

	// Valid (non-force) dashboard JWT.
	if session != nil {
		return true, 0
	}

	// requireLogin disabled → open (matches isAuthenticated()).
	if db := DBFromContext(r.Context()); db != nil {
		if settings, err := db.GetSettings(r.Context()); err == nil {
			if rl, isBool := settings["requireLogin"].(bool); isBool && !rl {
				return true, 0
			}
		}
	}

	return false, http.StatusUnauthorized
}

// Require gates a handler with allowed(). On denial it writes 401 Unauthorized,
// or 403 with a forcePasswordChange flag when a force-change session tried to
// reach a route outside the allow-list.
func (g *Guard) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, status := g.allowed(r)
		if ok {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		if status == http.StatusForbidden {
			_, _ = w.Write([]byte(`{"error":"Password change required","forcePasswordChange":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
	})
}
