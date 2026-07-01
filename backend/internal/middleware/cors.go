package middleware

import (
	"net/http"
	"strings"
)

// CORS adds permissive CORS headers for local development, matching the existing
// Node app behaviour (Access-Control-Allow-Origin: *). This is safe for the
// local-only default deployment; when the service is exposed to a network, auth
// middleware (Phase 3) gates access regardless of CORS.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", strings.Join([]string{
			"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS",
		}, ", "))
		// Allow all headers (mirrors current Node OPTIONS handlers). Sensitive
		// headers are still never logged by the Logging middleware.
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Allow-Credentials", "true")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
