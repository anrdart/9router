package httpapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/backend/internal/database"
	"9router/backend/internal/httpapi"
	"9router/backend/internal/middleware"
)

func openDB(t *testing.T) *database.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedSettings writes a settings blob directly.
func seedSettings(t *testing.T, db *database.DB, blob map[string]any) {
	t.Helper()
	data, _ := json.Marshal(blob)
	err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO settings(id, data) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data", string(data))
		return e
	})
	if err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

// TestSettingsGETStripsSecrets verifies password/oidcClientSecret are never returned.
func TestSettingsGETStripsSecrets(t *testing.T) {
	db := openDB(t)
	seedSettings(t, db, map[string]any{
		"requireLogin":     false,
		"password":         "super-secret-hash",
		"oidcClientSecret": "oidc-secret-value",
		"cloudEnabled":     true,
	})

	// Handler needs the DB in context.
	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.SettingsGET))

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "super-secret-hash") {
		t.Errorf("password leaked in response: %s", body)
	}
	if strings.Contains(body, "oidc-secret-value") {
		t.Errorf("oidcClientSecret leaked in response: %s", body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["cloudEnabled"] != true {
		t.Errorf("cloudEnabled = %v, want true", resp["cloudEnabled"])
	}
	if resp["hasPassword"] != true {
		t.Errorf("hasPassword = %v, want true", resp["hasPassword"])
	}
	// Env feature flags are always present (default false).
	if _, ok := resp["enableRequestLogs"]; !ok {
		t.Error("enableRequestLogs missing from settings response")
	}
	if _, ok := resp["enableTranslator"]; !ok {
		t.Error("enableTranslator missing from settings response")
	}
}

// TestSettingsGETEnvFlags verifies the env-derived flags reflect the environment.
func TestSettingsGETEnvFlags(t *testing.T) {
	t.Setenv("ENABLE_REQUEST_LOGS", "true")
	t.Setenv("ENABLE_TRANSLATOR", "false")
	db := openDB(t)
	seedSettings(t, db, map[string]any{"requireLogin": false})

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.SettingsGET))
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["enableRequestLogs"] != true {
		t.Errorf("enableRequestLogs = %v, want true", resp["enableRequestLogs"])
	}
	if resp["enableTranslator"] != false {
		t.Errorf("enableTranslator = %v, want false", resp["enableTranslator"])
	}
}

// TestSettingsGETHonorsMerge verifies defaults are present for unset keys.
func TestSettingsGETHonorsMerge(t *testing.T) {
	db := openDB(t)
	seedSettings(t, db, map[string]any{"requireLogin": false})

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.SettingsGET))
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["requireLogin"] != false {
		t.Errorf("requireLogin = %v, want false", resp["requireLogin"])
	}
	if resp["requireApiKey"] != true {
		t.Errorf("requireApiKey should default to true, got %v", resp["requireApiKey"])
	}
}
