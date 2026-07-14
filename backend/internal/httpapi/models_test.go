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

// ── seed helpers ──────────────────────────────────────────────────────────────

func seedKV(t *testing.T, db *database.DB, scope, key, value string) {
	t.Helper()
	err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT INTO kv(scope, key, value) VALUES(?,?,?)", scope, key, value)
		return e
	})
	if err != nil {
		t.Fatalf("seed kv %s/%s: %v", scope, key, err)
	}
}

func seedCombo(t *testing.T, db *database.DB, id, name, kind, createdAt string) {
	t.Helper()
	err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		var k any
		if kind != "" {
			k = kind
		}
		_, e := tx.Exec(
			"INSERT INTO combos(id, name, kind, models, createdAt, updatedAt) VALUES(?,?,?,?,?,?)",
			id, name, k, "[]", createdAt, createdAt,
		)
		return e
	})
	if err != nil {
		t.Fatalf("seed combo %s: %v", name, err)
	}
}

// fetchModels drives the native handler and returns the parsed data list. rp is a
// sentinel proxy that records whether it was invoked (i.e. the request should
// have been reverse-proxied to Node).
func fetchModels(t *testing.T, db *database.DB) ([]map[string]any, bool) {
	t.Helper()
	proxied := false
	rp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"PROXIED"}]}`))
	})

	h := middleware.WithDB(db)(httpapi.NewModelsHandler(rp, nil))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if proxied {
		return nil, true
	}

	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if resp.Object != "list" {
		t.Fatalf("object = %q, want \"list\"", resp.Object)
	}
	return resp.Data, false
}

func ids(models []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(models))
	for _, m := range models {
		if id, ok := m["id"].(string); ok {
			out[id] = m
		}
	}
	return out
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestModelsEmptyDBEnvelope: no connections → static-catalog branch. The response
// must be a valid {object:"list", data:[...]} with at least the known openai
// models, and data must be a JSON array (never null).
func TestModelsEmptyDBEnvelope(t *testing.T) {
	db := openDB(t)
	models, proxied := fetchModels(t, db)
	if proxied {
		t.Fatal("empty DB must be served natively, not proxied")
	}
	if len(models) == 0 {
		t.Fatal("static catalog should yield models")
	}
	// Invariant (survives catalog churn): the static branch exposes some openai/*
	// LLM, and every entry is a well-formed model object.
	foundOpenAI := false
	for id, m := range ids(models) {
		if strings.HasPrefix(id, "openai/") {
			foundOpenAI = true
		}
		if m["object"] != "model" || m["owned_by"] == nil {
			t.Errorf("malformed entry %s: %v", id, m)
		}
	}
	if !foundOpenAI {
		t.Errorf("expected some openai/* model in static catalog, got %d models", len(models))
	}
}

// TestModelsStaticExcludesNonLLM: the static branch must only emit llm-kind models
// (image/tts/embedding/stt are filtered for /v1/models).
func TestModelsStaticExcludesNonLLM(t *testing.T) {
	db := openDB(t)
	models, _ := fetchModels(t, db)
	// gemini image/embedding models exist in the snapshot; none may appear.
	for id := range ids(models) {
		// crude kind sniff on well-known non-llm suffixes present in snapshot
		for _, bad := range []string{"/embedding-001", "-flash-image", "/tts-1", "/whisper-1"} {
			if len(id) >= len(bad) && id[len(id)-len(bad):] == bad {
				t.Errorf("non-llm model leaked into /v1/models: %s", id)
			}
		}
	}
}

// TestModelsCombosFirstAndKindFilter: llm/kindless combos are emitted (owned_by
// "combo"); web combos are excluded by the llm filter.
func TestModelsCombosFirstAndKindFilter(t *testing.T) {
	db := openDB(t)
	seedCombo(t, db, "cb1", "my-llm-combo", "", "2026-01-01T00:00:00.000Z")
	seedCombo(t, db, "cb2", "my-web-combo", "webSearch", "2026-01-02T00:00:00.000Z")

	models, proxied := fetchModels(t, db)
	if proxied {
		t.Fatal("combos-only DB must be served natively")
	}
	byID := ids(models)
	c, ok := byID["my-llm-combo"]
	if !ok {
		t.Fatal("llm combo missing")
	}
	if c["owned_by"] != "combo" {
		t.Errorf("combo owned_by = %v, want combo", c["owned_by"])
	}
	if _, ok := byID["my-web-combo"]; ok {
		t.Error("webSearch combo must be excluded from /v1/models")
	}
	// combo must be first
	if len(models) > 0 && models[0]["id"] != "my-llm-combo" {
		t.Errorf("combo should be first, got %v", models[0]["id"])
	}
}

// TestModelsConnectionExplicitEnabled: an active openai connection with an
// explicit enabledModels allowlist yields exactly those ids (prefixed), served
// natively (no live resolution needed).
func TestModelsConnectionExplicitEnabled(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "openai", "OpenAI",
		`{"providerSpecificData":{"enabledModels":["gpt-4o","gpt-4o-mini"]}}`)

	models, proxied := fetchModels(t, db)
	if proxied {
		t.Fatal("explicit enabledModels must be served natively")
	}
	byID := ids(models)
	if _, ok := byID["openai/gpt-4o"]; !ok {
		t.Errorf("openai/gpt-4o missing; got %v", keysOf(byID))
	}
	if _, ok := byID["openai/gpt-4o-mini"]; !ok {
		t.Errorf("openai/gpt-4o-mini missing; got %v", keysOf(byID))
	}
}

// TestModelsPrefixOverride: providerSpecificData.prefix overrides the output
// alias for a connection's models.
func TestModelsPrefixOverride(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "openai", "OpenAI",
		`{"providerSpecificData":{"prefix":"myco","enabledModels":["gpt-4o"]}}`)

	models, proxied := fetchModels(t, db)
	if proxied {
		t.Fatal("must be native")
	}
	byID := ids(models)
	if _, ok := byID["myco/gpt-4o"]; !ok {
		t.Errorf("expected myco/gpt-4o (prefix override); got %v", keysOf(byID))
	}
	if _, ok := byID["openai/gpt-4o"]; ok {
		t.Error("static alias should not be used when prefix is set")
	}
}

// TestModelsDisabledFilter: a disabled model id for the output alias is excluded.
func TestModelsDisabledFilter(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "openai", "OpenAI",
		`{"providerSpecificData":{"enabledModels":["gpt-4o","gpt-4o-mini"]}}`)
	seedKV(t, db, "disabledModels", "openai", `["gpt-4o-mini"]`)

	models, _ := fetchModels(t, db)
	byID := ids(models)
	if _, ok := byID["openai/gpt-4o"]; !ok {
		t.Error("openai/gpt-4o should remain")
	}
	if _, ok := byID["openai/gpt-4o-mini"]; ok {
		t.Error("openai/gpt-4o-mini is disabled and must be excluded")
	}
}

// TestModelsCustomAndAliasMerge: a custom llm model and a model-alias target on
// an active provider are merged into its list. An imageToText custom model is
// kept (vision-capable chat) and carries capabilities.
func TestModelsCustomAndAliasMerge(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "openai", "OpenAI",
		`{"providerSpecificData":{"enabledModels":["gpt-4o"]}}`)
	seedKV(t, db, "customModels", "openai|my-custom|llm",
		`{"providerAlias":"openai","id":"my-custom","type":"llm","name":"My Custom"}`)
	seedKV(t, db, "customModels", "openai|vis-model|imageToText",
		`{"providerAlias":"openai","id":"vis-model","type":"imageToText","name":"Vis"}`)
	seedKV(t, db, "modelAliases", "shortcut", `"openai/aliased-model"`)

	models, proxied := fetchModels(t, db)
	if proxied {
		t.Fatal("must be native")
	}
	byID := ids(models)
	for _, want := range []string{"openai/gpt-4o", "openai/my-custom", "openai/vis-model", "openai/aliased-model"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("expected %s in merged list; got %v", want, keysOf(byID))
		}
	}
	vis := byID["openai/vis-model"]
	caps, ok := vis["capabilities"].(map[string]any)
	if !ok || caps["vision"] != true {
		t.Errorf("imageToText custom model should carry {vision:true}; got %v", vis["capabilities"])
	}
}

// TestModelsProxiesForLiveResolver: an active kiro connection with no explicit
// enabledModels requires a live catalog fetch → the request is reverse-proxied.
func TestModelsProxiesForLiveResolver(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "kiro", "Kiro", `{"accessToken":"at"}`)

	_, proxied := fetchModels(t, db)
	if !proxied {
		t.Fatal("kiro without enabledModels must be reverse-proxied to Node")
	}
}

// TestModelsKiroWithExplicitEnabledStaysNative: a kiro connection WITH an explicit
// allowlist does not need live resolution → served natively.
func TestModelsKiroWithExplicitEnabledStaysNative(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "kiro", "Kiro",
		`{"providerSpecificData":{"enabledModels":["claude-sonnet-4.5"]}}`)

	models, proxied := fetchModels(t, db)
	if proxied {
		t.Fatal("kiro WITH enabledModels should be native")
	}
	if _, ok := ids(models)["kr/claude-sonnet-4.5"]; !ok {
		// kiro's static alias is "kr"; the explicit id should appear under it.
		t.Logf("note: kiro output alias models = %v", keysOf(ids(models)))
	}
}

// TestModelsDedup: the same model reachable via static + alias appears once.
func TestModelsDedup(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "openai", "OpenAI",
		`{"providerSpecificData":{"enabledModels":["gpt-4o"]}}`)
	seedKV(t, db, "modelAliases", "dup", `"openai/gpt-4o"`)

	models, _ := fetchModels(t, db)
	count := 0
	for _, m := range models {
		if m["id"] == "openai/gpt-4o" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("openai/gpt-4o appears %d times, want 1 (dedup failed)", count)
	}
}

// TestModelsCustomKindDeterministic guards the ordering fix: when the same model
// id is registered as both llm and imageToText for one provider, the emitted
// entry's capabilities must be identical across repeated requests (KVScope is
// ordered, not map-random). Runs the build many times and requires one result.
func TestModelsCustomKindDeterministic(t *testing.T) {
	db := openDB(t)
	seedConn(t, db, "c1", "openai", "OpenAI",
		`{"providerSpecificData":{"enabledModels":["gpt-4o"]}}`)
	seedKV(t, db, "customModels", "openai|dup|imageToText",
		`{"providerAlias":"openai","id":"dup","type":"imageToText","name":"Dup"}`)
	seedKV(t, db, "customModels", "openai|dup|llm",
		`{"providerAlias":"openai","id":"dup","type":"llm","name":"Dup"}`)

	var first any
	seen := false
	for i := 0; i < 20; i++ {
		models, proxied := fetchModels(t, db)
		if proxied {
			t.Fatal("must be native")
		}
		entry, ok := ids(models)["openai/dup"]
		if !ok {
			t.Fatal("openai/dup missing")
		}
		caps := entry["capabilities"]
		capsJSON, _ := json.Marshal(caps)
		if !seen {
			first = string(capsJSON)
			seen = true
		} else if string(capsJSON) != first {
			t.Fatalf("capabilities flapped across requests: %q vs %q (KVScope not ordered?)", first, string(capsJSON))
		}
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
