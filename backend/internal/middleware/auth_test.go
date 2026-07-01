package middleware_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"9router/backend/internal/database"
	"9router/backend/internal/middleware"
)

func openDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestIsLocalRequest covers loopback vs non-loopback Host/Origin.
func TestIsLocalRequest(t *testing.T) {
	cases := []struct {
		host   string
		origin string
		want   bool
	}{
		{"localhost:20128", "", true},
		{"127.0.0.1:20128", "", true},
		{"[::1]:20128", "", true},
		{"example.com", "", false},
		{"203.0.113.5:20128", "", false},
		// A non-loopback Host is NOT rescued by a loopback Origin (matches Node:
		// the request still arrives over a non-loopback socket).
		{"203.0.113.5:20128", "http://localhost:3000", false},
		{"example.com", "https://evil.com", false},
		// Origin URLs are parsed too.
		{"localhost:20128", "https://evil.com", false},
		{"localhost:20128", "http://localhost:3000", true},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if got := middleware.IsLocalRequest(req); got != c.want {
			t.Errorf("IsLocalRequest(host=%q origin=%q) = %v, want %v", c.host, c.origin, got, c.want)
		}
	}
}

// TestExtractAPIKey covers the supported header/query variants.
func TestExtractAPIKey(t *testing.T) {
	cases := []struct {
		name string
		hdr  map[string]string
		url  string
		want string
	}{
		{"bearer", map[string]string{"Authorization": "Bearer sk-123"}, "/x", "sk-123"},
		{"x-api-key", map[string]string{"X-Api-Key": "sk-456"}, "/x", "sk-456"},
		{"x-goog-api-key", map[string]string{"X-Goog-Api-Key": "goog-789"}, "/x", "goog-789"},
		{"query key", map[string]string{}, "/x?key=qk-1", "qk-1"},
		{"none", map[string]string{}, "/x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.url, nil)
			for k, v := range c.hdr {
				req.Header.Set(k, v)
			}
			if got := middleware.ExtractAPIKey(req); got != c.want {
				t.Errorf("ExtractAPIKey = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRequireDashboardAuthLocal verifies local requests pass without a key.
func TestRequireDashboardAuthLocal(t *testing.T) {
	db := openDB(t)
	called := false
	h := middleware.WithDB(db)(middleware.RequireDashboardAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Error("local request should reach handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestRequireDashboardAuthRemoteNoKey verifies non-local requests without a key get 401.
func TestRequireDashboardAuthRemoteNoKey(t *testing.T) {
	db := openDB(t)
	called := false
	h := middleware.WithDB(db)(middleware.RequireDashboardAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})))

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Host = "203.0.113.5"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Error("non-local request without key should NOT reach handler")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestRequireDashboardAuthRemoteWithKey verifies a valid API key grants access.
func TestRequireDashboardAuthRemoteWithKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	err := db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO apiKeys(id, key, name, machineId, isActive, createdAt) VALUES(?,?,?,?,?,?)",
			"k1", "sk-valid", "t", "m1", 1, "2026-01-01T00:00:00.000Z")
		return e
	})
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}

	called := false
	h := middleware.WithDB(db)(middleware.RequireDashboardAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Host = "203.0.113.5"
	req.Header.Set("Authorization", "Bearer sk-valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Error("non-local request with valid key should reach handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
