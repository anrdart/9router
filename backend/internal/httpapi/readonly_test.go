package httpapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"9router/backend/internal/database"
	"9router/backend/internal/httpapi"
	"9router/backend/internal/middleware"
)

// seedConn inserts a provider connection with an opaque data blob.
func seedConn(t *testing.T, db *database.DB, id, provider, name, data string) {
	t.Helper()
	err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt) VALUES(?,?,?,?,?,?,?,?,?,?)",
			id, provider, "oauth", name, "u@x.com", 1, 1, data, "2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00.000Z",
		)
		return e
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}
}

// TestProvidersGETRedactsSecrets is the D1 regression guard: OAuth tokens and API
// keys in the data blob must never leave the /api/providers handler, and the
// response must be wrapped as {connections:[...]}.
func TestProvidersGETRedactsSecrets(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "openai", "My OpenAI",
		`{"apiKey":"sk-SECRET","accessToken":"at-SECRET","refreshToken":"rt-SECRET","idToken":"id-SECRET","email":"u@x.com"}`)

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.ProvidersGET))
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"sk-SECRET", "at-SECRET", "rt-SECRET", "id-SECRET"} {
		if strings.Contains(body, secret) {
			t.Errorf("secret %q leaked in response: %s", secret, body)
		}
	}

	var resp struct {
		Connections []map[string]any `json:"connections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Connections) != 1 {
		t.Fatalf("connections = %d, want 1 (envelope missing?)", len(resp.Connections))
	}
	c := resp.Connections[0]
	for _, f := range []string{"apiKey", "accessToken", "refreshToken", "idToken"} {
		if _, present := c[f]; present {
			t.Errorf("field %q should be stripped, present in %v", f, c)
		}
	}
	// Non-secret fields survive.
	if c["id"] != "c1" || c["provider"] != "openai" {
		t.Errorf("expected non-secret fields intact, got %v", c)
	}
}

// TestProvidersGETEnrichesCompatibleName verifies compatible-provider names fall
// back to the providerNodes name when the connection name is empty.
func TestProvidersGETEnrichesCompatibleName(t *testing.T) {
	db := openDB(t)
	// providerNodes row id must equal the connection's provider id.
	err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO providerNodes(id, type, name, data, createdAt, updatedAt) VALUES(?,?,?,?,?,?)",
			"openai-compatible-acme", "openai-compatible", "Acme Gateway", "{}", "t", "t",
		)
		return e
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	// Connection with empty name → should inherit "Acme Gateway".
	err = db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO providerConnections(id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt) VALUES(?,?,?,?,?,?,?,?,?,?)",
			"c2", "openai-compatible-acme", "apikey", nil, nil, 1, 1, "{}", "t", "t",
		)
		return e
	})
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.ProvidersGET))
	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Connections []map[string]any `json:"connections"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Connections) != 1 {
		t.Fatalf("connections = %d, want 1", len(resp.Connections))
	}
	if got := resp.Connections[0]["name"]; got != "Acme Gateway" {
		t.Errorf("enriched name = %v, want %q", got, "Acme Gateway")
	}
}

// seedUsage inserts one usageHistory row.
func seedUsage(t *testing.T, db *database.DB, ts, provider, model, apiKey string, prompt, completion int) {
	t.Helper()
	err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO usageHistory(timestamp, provider, model, apiKey, promptTokens, completionTokens, cost, status, tokens) VALUES(?,?,?,?,?,?,?,?,?)",
			ts, provider, model, apiKey, prompt, completion, 0.0, "200",
			`{"prompt_tokens":`+itoa(prompt)+`,"completion_tokens":`+itoa(completion)+`}`,
		)
		return e
	})
	if err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestUsageLogsGETNoKeyLeak is the D2 regression guard: the raw apiKey must never
// appear in /api/usage/logs, and the result is a JSON array of strings.
func TestUsageLogsGETNoKeyLeak(t *testing.T) {
	db := openDB(t)
	seedUsage(t, db, "2026-06-01T10:00:00.000Z", "openai", "gpt-4", "sk-LEAKME", 10, 5)

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.UsageLogsGET))
	req := httptest.NewRequest(http.MethodGet, "/api/usage/logs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-LEAKME") {
		t.Errorf("raw apiKey leaked: %s", body)
	}
	var logs []string
	if err := json.Unmarshal(rec.Body.Bytes(), &logs); err != nil {
		t.Fatalf("expected JSON string array, got %s (%v)", body, err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "gpt-4") || !strings.Contains(logs[0], "OPENAI") {
		t.Errorf("unexpected logs: %v", logs)
	}
}

// TestUsageStatsGETShape verifies aggregation shape, masking, and the empty live
// fields for the live-history (24h) path.
func TestUsageStatsGETShape(t *testing.T) {
	db := openDB(t)
	// Use a timestamp "now-ish" so the 24h window includes it.
	ts := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	seedUsage(t, db, ts, "openai", "gpt-4", "sk-verysecret-abcdef", 100, 50)

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.UsageStatsGET))
	req := httptest.NewRequest(http.MethodGet, "/api/usage/stats?period=24h", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-verysecret-abcdef") {
		t.Errorf("raw apiKey leaked in stats: %s", body)
	}

	var stats map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stats["totalRequests"].(float64) != 1 {
		t.Errorf("totalRequests = %v, want 1", stats["totalRequests"])
	}
	if stats["totalPromptTokens"].(float64) != 100 {
		t.Errorf("totalPromptTokens = %v, want 100", stats["totalPromptTokens"])
	}
	// Live fields present but empty.
	if ar, ok := stats["activeRequests"].([]any); !ok || len(ar) != 0 {
		t.Errorf("activeRequests = %v, want empty array", stats["activeRequests"])
	}
	if stats["errorProvider"] != "" {
		t.Errorf("errorProvider = %v, want empty", stats["errorProvider"])
	}
	// byApiKey should be masked (prefix + ***), not raw.
	byKey, _ := stats["byApiKey"].(map[string]any)
	if len(byKey) == 0 {
		t.Fatal("byApiKey empty")
	}
	for _, v := range byKey {
		e := v.(map[string]any)
		if m, _ := e["apiKeyMasked"].(string); !strings.HasSuffix(m, "***") {
			t.Errorf("apiKeyMasked = %v, want masked", e["apiKeyMasked"])
		}
	}
}

// TestUsageStatsGETInvalidPeriod verifies period validation.
func TestUsageStatsGETInvalidPeriod(t *testing.T) {
	db := openDB(t)
	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.UsageStatsGET))
	req := httptest.NewRequest(http.MethodGet, "/api/usage/stats?period=bogus", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestUsageStatsGETDailyPath exercises the usageDaily aggregation path (7d).
func TestUsageStatsGETDailyPath(t *testing.T) {
	db := openDB(t)
	// Seed a usageDaily row for today's local date key.
	dateKey := time.Now().Local().Format("2006-01-02")
	dayBlob := `{"requests":3,"promptTokens":300,"completionTokens":150,"cost":0.5,` +
		`"byProvider":{"openai":{"requests":3,"promptTokens":300,"completionTokens":150,"cost":0.5}},` +
		`"byModel":{"gpt-4|openai":{"requests":3,"promptTokens":300,"completionTokens":150,"cost":0.5,"rawModel":"gpt-4","provider":"openai"}},` +
		`"byApiKey":{"sk-abc|gpt-4|openai":{"requests":3,"promptTokens":300,"completionTokens":150,"cost":0.5,"rawModel":"gpt-4","provider":"openai","apiKey":"sk-abcdefghij"}}}`
	err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO usageDaily(dateKey, data) VALUES(?, ?)", dateKey, dayBlob)
		return e
	})
	if err != nil {
		t.Fatalf("seed daily: %v", err)
	}

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.UsageStatsGET))
	req := httptest.NewRequest(http.MethodGet, "/api/usage/stats?period=7d", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-abcdefghij") {
		t.Errorf("raw apiKey leaked from daily blob: %s", body)
	}
	var stats map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &stats)
	if stats["totalRequests"].(float64) != 3 {
		t.Errorf("totalRequests = %v, want 3", stats["totalRequests"])
	}
	if stats["totalPromptTokens"].(float64) != 300 {
		t.Errorf("totalPromptTokens = %v, want 300", stats["totalPromptTokens"])
	}
}

// TestAPIKeysGETEnvelope verifies the {keys:[...]} envelope.
func TestAPIKeysGETEnvelope(t *testing.T) {
	db := openDB(t)
	if err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO apiKeys(id, key, name, machineId, isActive, createdAt) VALUES(?,?,?,?,?,?)",
			"k1", "sk-abc", "test", "m1", 1, "2026-01-01T00:00:00.000Z")
		return e
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	h := middleware.WithDB(db)(http.HandlerFunc(httpapi.APIKeysGET))
	req := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Keys) != 1 || resp.Keys[0]["id"] != "k1" {
		t.Errorf("keys envelope wrong: %v", resp.Keys)
	}
}
