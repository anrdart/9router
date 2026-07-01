package middleware_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// --- Guard (Phase 3 auth parity) --------------------------------------------

// jwtSecret is the shared HMAC secret; write it under a temp DATA_DIR so the
// guard reads it exactly like Node.
const jwtSecret = "test-jwt-secret"

// setupGuardDir returns a DATA_DIR seeded with jwt-secret and (optionally) the
// CLI token files, plus the derived CLI token value.
func setupGuardDir(t *testing.T, withCLI bool) (dir, cliToken string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "jwt-secret"), []byte(jwtSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withCLI {
		if err := os.WriteFile(filepath.Join(dir, "machine-id"), []byte("machine-xyz\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		authDir := filepath.Join(dir, "auth")
		if err := os.MkdirAll(authDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(authDir, "cli-secret"), []byte("cli-secret-xyz\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("machine-xyz" + "9r-cli-auth" + "cli-secret-xyz"))
		cliToken = hex.EncodeToString(sum[:])[:16]
	}
	return dir, cliToken
}

// mintJWT builds an HS256 auth_token the guard will accept.
func mintJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	signing := enc(header) + "." + enc(body)
	m := hmac.New(sha256.New, []byte(jwtSecret))
	m.Write([]byte(signing))
	return signing + "." + enc(m.Sum(nil))
}

// serve runs a request through guard.Require and reports whether it reached the
// handler and the response status.
func serve(t *testing.T, guard *middleware.Guard, db *database.DB, req *http.Request) (called bool, code int) {
	t.Helper()
	h := middleware.WithDB(db)(guard.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return called, rec.Code
}

// TestGuardNoCredential verifies a request with no cookie/CLI token is rejected,
// even from a loopback Host (the old locality bypass is gone).
func TestGuardNoCredential(t *testing.T) {
	dir, _ := setupGuardDir(t, false)
	db := openDB(t)
	guard := middleware.NewGuard(dir)

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Host = "127.0.0.1" // loopback must NOT grant access
	called, code := serve(t, guard, db, req)
	if called {
		t.Error("request without credential should not reach handler")
	}
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// TestGuardLLMKeyRejected verifies an inbound LLM API key does NOT grant
// dashboard access (privilege escalation closed).
func TestGuardLLMKeyRejected(t *testing.T) {
	dir, _ := setupGuardDir(t, false)
	db := openDB(t)
	ctx := context.Background()
	if err := db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO apiKeys(id, key, name, machineId, isActive, createdAt) VALUES(?,?,?,?,?,?)",
			"k1", "sk-valid", "t", "m1", 1, "2026-01-01T00:00:00.000Z")
		return e
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	guard := middleware.NewGuard(dir)

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Host = "203.0.113.5"
	req.Header.Set("Authorization", "Bearer sk-valid")
	called, code := serve(t, guard, db, req)
	if called {
		t.Error("LLM API key must not grant dashboard access")
	}
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// TestGuardValidJWT verifies a valid dashboard session cookie grants access.
func TestGuardValidJWT(t *testing.T) {
	dir, _ := setupGuardDir(t, false)
	db := openDB(t)
	guard := middleware.NewGuard(dir)

	tok := mintJWT(t, map[string]any{"authenticated": true, "exp": farFuture()})
	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Host = "203.0.113.5"
	req.AddCookie(&http.Cookie{Name: "auth_token", Value: tok})
	called, code := serve(t, guard, db, req)
	if !called || code != http.StatusOK {
		t.Errorf("valid JWT should reach handler: called=%v code=%d", called, code)
	}
}

// TestGuardExpiredJWT verifies an expired cookie is rejected.
func TestGuardExpiredJWT(t *testing.T) {
	dir, _ := setupGuardDir(t, false)
	db := openDB(t)
	guard := middleware.NewGuard(dir)

	tok := mintJWT(t, map[string]any{"authenticated": true, "exp": int64(1)})
	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.AddCookie(&http.Cookie{Name: "auth_token", Value: tok})
	called, code := serve(t, guard, db, req)
	if called || code != http.StatusUnauthorized {
		t.Errorf("expired JWT must be rejected: called=%v code=%d", called, code)
	}
}

// TestGuardCLIToken verifies the CLI machine token grants access.
func TestGuardCLIToken(t *testing.T) {
	dir, cliToken := setupGuardDir(t, true)
	db := openDB(t)
	guard := middleware.NewGuard(dir)

	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	req.Host = "203.0.113.5"
	req.Header.Set("X-9R-Cli-Token", cliToken)
	called, code := serve(t, guard, db, req)
	if !called || code != http.StatusOK {
		t.Errorf("valid CLI token should reach handler: called=%v code=%d", called, code)
	}
}

// TestGuardRequireLoginDisabled verifies requireLogin=false opens the gate.
func TestGuardRequireLoginDisabled(t *testing.T) {
	dir, _ := setupGuardDir(t, false)
	db := openDB(t)
	seedRequireLogin(t, db, false)
	guard := middleware.NewGuard(dir)

	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.Host = "203.0.113.5"
	called, code := serve(t, guard, db, req)
	if !called || code != http.StatusOK {
		t.Errorf("requireLogin=false should open gate: called=%v code=%d", called, code)
	}
}

// TestGuardForcePasswordChange verifies a force-change session reaches only
// /api/settings (403 elsewhere).
func TestGuardForcePasswordChange(t *testing.T) {
	dir, _ := setupGuardDir(t, false)
	db := openDB(t)
	guard := middleware.NewGuard(dir)

	tok := mintJWT(t, map[string]any{"authenticated": true, "force_password_change": true, "exp": farFuture()})

	// /api/keys → 403
	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	req.AddCookie(&http.Cookie{Name: "auth_token", Value: tok})
	called, code := serve(t, guard, db, req)
	if called || code != http.StatusForbidden {
		t.Errorf("force-change on /api/keys: called=%v code=%d, want 403", called, code)
	}

	// /api/settings → 200
	req2 := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req2.AddCookie(&http.Cookie{Name: "auth_token", Value: tok})
	called2, code2 := serve(t, guard, db, req2)
	if !called2 || code2 != http.StatusOK {
		t.Errorf("force-change on /api/settings: called=%v code=%d, want 200", called2, code2)
	}
}

// farFuture returns an exp comfortably ahead of now.
func farFuture() int64 { return 4_102_444_800 } // 2100-01-01

// seedRequireLogin writes a settings blob with the given requireLogin value.
func seedRequireLogin(t *testing.T, db *database.DB, requireLogin bool) {
	t.Helper()
	blob, _ := json.Marshal(map[string]any{"requireLogin": requireLogin})
	if err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO settings(id, data) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data", string(blob))
		return e
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}
