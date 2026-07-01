// Package httpapi contains the Go-backend HTTP handlers for endpoints that are
// implemented natively (not reverse-proxied).
package httpapi

import (
	"encoding/json"
	"net/http"
)

// Health responds with {"ok": true}. This is the canonical liveness probe shared
// with the Node app (src/app/api/health/route.js) so dashboards and load
// balancers can hit either transparently.
func Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
