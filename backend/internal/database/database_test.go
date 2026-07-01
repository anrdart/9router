package database_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"9router/backend/internal/database"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openTempDB(t *testing.T) *database.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(context.Background(), dir, silentLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestOpenCreatesDir verifies the db/ subdirectory is created when missing.
func TestOpenCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "missing")
	db, err := database.Open(context.Background(), dir, silentLogger())
	if err != nil {
		t.Fatalf("Open should create dirs: %v", err)
	}
	db.Close()
}

// TestPathFallback verifies Path falls back to a non-empty path when dataDir is empty.
func TestPathFallback(t *testing.T) {
	p, err := database.Path("")
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p == "" {
		t.Fatal("Path should not be empty")
	}
}

// TestSchemaSyncIdempotent verifies opening a DB twice does not error.
func TestSchemaSyncIdempotent(t *testing.T) {
	dir := t.TempDir()
	db1, err := database.Open(context.Background(), dir, silentLogger())
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	db1.Close()

	db2, err := database.Open(context.Background(), dir, silentLogger())
	if err != nil {
		t.Fatalf("second Open (idempotent): %v", err)
	}
	db2.Close()
}

// TestSettingsDefaults verifies an empty DB returns merged default settings.
func TestSettingsDefaults(t *testing.T) {
	db := openTempDB(t)
	s, err := db.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s["requireLogin"] != true {
		t.Errorf("requireLogin default = %v, want true", s["requireLogin"])
	}
	if s["requireApiKey"] != true {
		t.Errorf("requireApiKey default = %v, want true", s["requireApiKey"])
	}
}

// TestSettingsRoundtrip verifies writing then reading settings persists and merges.
func TestSettingsRoundtrip(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	custom := map[string]any{"requireLogin": false, "cloudEnabled": true, "customKey": "x"}
	data, _ := json.Marshal(custom)
	err := db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"INSERT INTO settings(id, data) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data",
			string(data),
		)
		return err
	})
	if err != nil {
		t.Fatalf("write settings: %v", err)
	}

	s, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s["requireLogin"] != false {
		t.Errorf("requireLogin = %v, want false (overridden)", s["requireLogin"])
	}
	if s["cloudEnabled"] != true {
		t.Errorf("cloudEnabled = %v, want true (overridden)", s["cloudEnabled"])
	}
	if s["customKey"] != "x" {
		t.Errorf("customKey = %v, want x", s["customKey"])
	}
	// Defaults not overridden must still be present.
	if s["requireApiKey"] != true {
		t.Errorf("requireApiKey default should persist = %v", s["requireApiKey"])
	}
}

// TestValidateApiKey verifies key validation in constant time.
func TestValidateApiKey(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	// Insert two keys, one inactive.
	err := db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO apiKeys(id, key, name, machineId, isActive, createdAt) VALUES(?,?,?, ?,?,?)",
			"k1", "sk-active-secret", "test", "m1", 1, "2026-01-01T00:00:00.000Z")
		if e != nil {
			return e
		}
		_, e = tx.Exec("INSERT INTO apiKeys(id, key, name, machineId, isActive, createdAt) VALUES(?,?,?, ?,?,?)",
			"k2", "sk-inactive-secret", "test", "m2", 0, "2026-01-01T00:00:00.000Z")
		return e
	})
	if err != nil {
		t.Fatalf("insert keys: %v", err)
	}

	cases := []struct {
		name   string
		key    string
		wantOK bool
	}{
		{"active key", "sk-active-secret", true},
		{"inactive key", "sk-inactive-secret", false},
		{"unknown key", "sk-unknown", false},
		{"empty key", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, err := db.ValidateApiKey(ctx, c.key)
			if err != nil {
				t.Fatalf("ValidateApiKey: %v", err)
			}
			if ok != c.wantOK {
				t.Errorf("ValidateApiKey(%q) = %v, want %v", c.key, ok, c.wantOK)
			}
		})
	}
}

// TestListProviderConnections verifies connection rows are returned with merged data.
func TestListProviderConnections(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	// Insert a connection with an opaque data blob.
	err := db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt) VALUES(?,?,?,?,?,?,?,?,?,?)",
			"conn-1", "openai", "api-key", "My Key", "u@x.com", 1, 1,
			`{"apiKey":"sk-xxx","providerSpecificData":{}}`, "2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00.000Z",
		)
		return e
	})
	if err != nil {
		t.Fatalf("insert connection: %v", err)
	}

	conns, err := db.ListProviderConnections(ctx, "", false)
	if err != nil {
		t.Fatalf("ListProviderConnections: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want 1", len(conns))
	}

	j, err := conns[0].ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	if j["id"] != "conn-1" {
		t.Errorf("id = %v", j["id"])
	}
	if j["provider"] != "openai" {
		t.Errorf("provider = %v", j["provider"])
	}
	if j["isActive"] != true {
		t.Errorf("isActive = %v", j["isActive"])
	}
	// The DB layer intentionally merges the full opaque data blob (including
	// secrets) — faithful round-trip is the repo's job. Secret REDACTION happens
	// at the HTTP boundary (httpapi.ProvidersGET); see TestProvidersGETRedactsSecrets.
	if j["apiKey"] != "sk-xxx" {
		t.Errorf("apiKey from data blob = %v, want sk-xxx", j["apiKey"])
	}
}

// TestListProviderConnectionsFilter verifies the provider/active filters.
func TestListProviderConnectionsFilter(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	err := db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		stmts := []string{
			"INSERT INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt) VALUES('a','openai','x',NULL,NULL,1,1,'{}','t','t')",
			"INSERT INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt) VALUES('b','openai','x',NULL,NULL,2,0,'{}','t','t')",
			"INSERT INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt) VALUES('c','anthropic','x',NULL,NULL,1,1,'{}','t','t')",
		}
		for _, s := range stmts {
			if _, e := tx.Exec(s); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	all, _ := db.ListProviderConnections(ctx, "", false)
	if len(all) != 3 {
		t.Errorf("all = %d, want 3", len(all))
	}

	openai, _ := db.ListProviderConnections(ctx, "openai", false)
	if len(openai) != 2 {
		t.Errorf("openai = %d, want 2", len(openai))
	}

	activeOnly, _ := db.ListProviderConnections(ctx, "", true)
	if len(activeOnly) != 2 {
		t.Errorf("activeOnly = %d, want 2", len(activeOnly))
	}
}

// TestRecentLogs verifies logs come back newest-first as formatted strings and
// never contain the raw API key (getRecentLogs does not select it).
func TestRecentLogs(t *testing.T) {
	db := openTempDB(t)
	ctx := context.Background()

	err := db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		for i, ts := range []string{
			"2026-01-01T10:00:00.000Z", "2026-01-02T10:00:00.000Z", "2026-01-03T10:00:00.000Z",
		} {
			if _, e := tx.Exec(
				"INSERT INTO usageHistory(timestamp, provider, model, apiKey, promptTokens, completionTokens, status) VALUES(?,?,?,?,?,?,?)",
				ts, "openai", "gpt-4", "sk-SECRET-KEY", i*10, i*5, "200",
			); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert usage: %v", err)
	}

	logs, err := db.RecentLogs(ctx, 10)
	if err != nil {
		t.Fatalf("RecentLogs: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("got %d logs, want 3", len(logs))
	}
	// Newest first: the 03 row must render before the 01 row.
	if !strings.Contains(logs[0], "03-01-2026") {
		t.Errorf("first log = %q, want newest (03-01-2026)", logs[0])
	}
	// The raw API key must never appear.
	for _, l := range logs {
		if strings.Contains(l, "sk-SECRET-KEY") {
			t.Errorf("raw apiKey leaked in log line: %q", l)
		}
		// Node uppercases only the provider, not the model.
		if !strings.Contains(l, "gpt-4") || !strings.Contains(l, "OPENAI") {
			t.Errorf("log line missing expected fields: %q", l)
		}
	}
}
