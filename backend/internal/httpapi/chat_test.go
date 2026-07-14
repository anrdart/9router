package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"9router/backend/internal/database"
	"9router/backend/internal/middleware"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func openChatDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedChatSettings(t *testing.T, db *database.DB, settings map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(settings)
	if err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO settings(id, data) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(raw))
		return err
	}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

func seedChatConn(t *testing.T, db *database.DB, id, data string, priority int) {
	seedProviderChatConn(t, db, id, "groq", data, priority)
}

func seedProviderChatConn(t *testing.T, db *database.DB, id, provider, data string, priority int) {
	t.Helper()
	if err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO providerConnections
			(id, provider, authType, name, priority, isActive, data, createdAt, updatedAt)
			VALUES(?, ?, 'apikey', ?, ?, 1, ?, ?, ?)`,
			id, provider, provider, priority, data, "2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00.000Z")
		return err
	}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
}

func seedChatCombo(t *testing.T, db *database.DB, name string, models []string) {
	t.Helper()
	raw, _ := json.Marshal(models)
	if err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO combos(id, name, kind, models, createdAt, updatedAt) VALUES(?, ?, 'llm', ?, ?, ?)`,
			"combo-"+name, name, string(raw), "2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00.000Z")
		return err
	}); err != nil {
		t.Fatalf("seed combo: %v", err)
	}
}

func chatRequest(t *testing.T, h http.Handler, raw string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func chatHarness(db *database.DB, doer httpDoer) (http.Handler, *int, *string) {
	proxied := 0
	proxiedBody := ""
	rp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied++
		raw, _ := io.ReadAll(r.Body)
		proxiedBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"proxied":true}`)
	})
	return middleware.WithDB(db)(newChatHandler(rp, nil, doer)), &proxied, &proxiedBody
}

func response(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestNativeChatJSONNormalizesAndPersistsRawUsage(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false, "enableObservability2": true})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream-secret","providerSpecificData":{}}`, 1)

	var upstreamBody map[string]any
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.groq.com/openai/v1/chat/completions" {
			t.Errorf("URL = %q", req.URL.String())
		}
		if got := req.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewDecoder(req.Body).Decode(&upstreamBody)
		return response(http.StatusOK, "application/json", `{
			"id":"chatcmpl-1","model":"llama-3.3-70b-versatile",
			"prompt_filter_results":[{}],
			"choices":[{"message":{"content":"ok","reasoning_content":"secret-thought","tool_calls":[{"id":"call-1"}]},"finish_reason":"other","content_filter_results":{}}],
			"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,
				"prompt_tokens_details":{"cached_tokens":30,"cache_creation_tokens":5},
				"completion_tokens_details":{"reasoning_tokens":7}}
		}`), nil
	})
	h, proxied, _ := chatHarness(db, doer)
	raw := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"temperature":0.2,"messages":[{"role":"user","content":"hello"}]}`
	rec := chatRequest(t, h, raw, nil)

	if *proxied != 0 || rec.Code != http.StatusOK {
		t.Fatalf("proxied=%d status=%d body=%s", *proxied, rec.Code, rec.Body.String())
	}
	if upstreamBody["model"] != "llama-3.3-70b-versatile" {
		t.Errorf("upstream model = %v", upstreamBody["model"])
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["prompt_filter_results"]; present {
		t.Error("prompt_filter_results leaked")
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" || choice["content_filter_results"] != nil {
		t.Errorf("choice normalization failed: %v", choice)
	}
	msg := choice["message"].(map[string]any)
	if _, present := msg["reasoning_content"]; present {
		t.Error("reasoning_content should be stripped when content is present")
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(2100) || usage["total_tokens"] != float64(2120) {
		t.Errorf("client usage buffer wrong: %v", usage)
	}

	var prompt, completion int64
	var tokensRaw string
	if err := db.QueryRow("SELECT promptTokens, completionTokens, tokens FROM usageHistory").Scan(&prompt, &completion, &tokensRaw); err != nil {
		t.Fatalf("usageHistory: %v", err)
	}
	if prompt != 100 || completion != 20 {
		t.Errorf("stored usage = %d/%d", prompt, completion)
	}
	var stored database.ChatUsage
	_ = json.Unmarshal([]byte(tokensRaw), &stored)
	if stored.Cached != 30 || stored.Reasoning != 7 || stored.CacheCreation != 5 {
		t.Errorf("stored usage details = %+v", stored)
	}
	var dayCount, detailCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM usageDaily").Scan(&dayCount)
	_ = db.QueryRow("SELECT COUNT(*) FROM requestDetails").Scan(&detailCount)
	if dayCount != 1 || detailCount != 1 {
		t.Errorf("daily=%d details=%d", dayCount, detailCount)
	}
}

func TestNativeChatFailClosedProxyPreservesBody(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		conns []string
	}{
		{"unknown provider", `{"model":"unknown/model","stream":false,"messages":[{"role":"user","content":"x"}]}`, []string{`{"apiKey":"upstream"}`}},
		{"unknown model", `{"model":"groq/not-catalogued","stream":false,"messages":[{"role":"user","content":"x"}]}`, []string{`{"apiKey":"upstream"}`}},
		{"stream omitted", `{"model":"groq/llama-3.3-70b-versatile","messages":[{"role":"user","content":"x"}]}`, []string{`{"apiKey":"upstream"}`}},
		{"content blocks", `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`, []string{`{"apiKey":"upstream"}`}},
		{"tool turn", `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"assistant","content":"","tool_calls":[{}]}]}`, []string{`{"apiKey":"upstream"}`}},
		{"thinking transform", `{"model":"groq/qwen/qwen3-32b","stream":false,"reasoning_effort":"high","messages":[{"role":"user","content":"x"}]}`, []string{`{"apiKey":"upstream"}`}},
		{"client metadata", `{"model":"groq/llama-3.3-70b-versatile","stream":false,"client_metadata":{"x":1},"messages":[{"role":"user","content":"x"}]}`, []string{`{"apiKey":"upstream"}`}},
		{"oauth refresh", `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`, []string{`{"accessToken":"a","refreshToken":"r"}`}},
		{"connection proxy", `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`, []string{`{"apiKey":"a","providerSpecificData":{"connectionProxyEnabled":true}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openChatDB(t)
			seedChatSettings(t, db, map[string]any{"requireApiKey": false})
			for i, data := range tc.conns {
				seedChatConn(t, db, "conn-"+string(rune('a'+i)), data, i+1)
			}
			doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("native upstream called")
				return nil, nil
			})
			h, proxied, proxiedBody := chatHarness(db, doer)
			rec := chatRequest(t, h, tc.body, nil)
			if rec.Code != http.StatusOK || *proxied != 1 || *proxiedBody != tc.body {
				t.Fatalf("status=%d proxied=%d body=%q", rec.Code, *proxied, *proxiedBody)
			}
		})
	}
}

func TestNativeChatAuthUsesOnlyChatKeySources(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": true})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream"}`, 1)
	if err := db.WithWriteLock(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO apiKeys(id, key, name, isActive, createdAt) VALUES('key-1','client-valid','test',1,'2026-01-01')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	for name, headers := range map[string]map[string]string{
		"missing": nil,
		"invalid": {"Authorization": "Bearer bad"},
		"goog":    {"X-Goog-Api-Key": "client-valid"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := chatRequest(t, h, body, headers)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	for _, headers := range []map[string]string{{"Authorization": "Bearer client-valid"}, {"X-Api-Key": "client-valid"}} {
		if rec := chatRequest(t, h, body, headers); rec.Code != http.StatusOK {
			t.Fatalf("valid key status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if calls != 2 {
		t.Errorf("upstream calls=%d, want 2", calls)
	}
}

func TestNativeChatMultiAccountFillFirstFallback(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-low", `{"apiKey":"first"}`, 1)
	seedChatConn(t, db, "conn-high", `{"apiKey":"second"}`, 2)

	var calls []string
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		auth := req.Header.Get("Authorization")
		calls = append(calls, auth)
		if auth == "Bearer first" {
			return response(http.StatusTooManyRequests, "application/json", `{"error":{"message":"rate limit"}}`), nil
		}
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	h, proxied, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusOK || *proxied != 0 {
		t.Fatalf("status=%d proxied=%d body=%s", rec.Code, *proxied, rec.Body.String())
	}
	if strings.Join(calls, ",") != "Bearer first,Bearer second" {
		t.Errorf("call order=%v", calls)
	}
	var raw string
	_ = db.QueryRow("SELECT data FROM providerConnections WHERE id='conn-low'").Scan(&raw)
	if !strings.Contains(raw, "modelLock_llama-3.3-70b-versatile") {
		t.Errorf("first account not locked: %s", raw)
	}
}

func TestNativeChatMultiAccountNoFallbackOn400(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-a", `{"apiKey":"first"}`, 1)
	seedChatConn(t, db, "conn-b", `{"apiKey":"second"}`, 2)
	calls := 0
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusBadRequest, "application/json", `{"error":{"message":"bad input"}}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	var locks int
	_ = db.QueryRow("SELECT COUNT(*) FROM providerConnections WHERE data LIKE '%modelLock_%'").Scan(&locks)
	if locks != 0 {
		t.Errorf("400 locked %d accounts", locks)
	}
}

func TestNativeChatRoundRobinSticky(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{
		"requireApiKey":      false,
		"providerStrategies": map[string]any{"groq": map[string]any{"fallbackStrategy": "round-robin", "stickyRoundRobinLimit": 2}},
	})
	seedChatConn(t, db, "conn-a", `{"apiKey":"one"}`, 1)
	seedChatConn(t, db, "conn-b", `{"apiKey":"two"}`, 2)
	var calls []string
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Header.Get("Authorization"))
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	for range 3 {
		if rec := chatRequest(t, h, body, nil); rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if strings.Join(calls, ",") != "Bearer one,Bearer one,Bearer two" {
		t.Errorf("sticky call order=%v", calls)
	}
}

func TestNativeChatRoundRobinConcurrentSelection(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{
		"requireApiKey":      false,
		"providerStrategies": map[string]any{"groq": map[string]any{"fallbackStrategy": "round-robin", "stickyRoundRobinLimit": 1}},
	})
	seedChatConn(t, db, "conn-a", `{"apiKey":"one"}`, 1)
	seedChatConn(t, db, "conn-b", `{"apiKey":"two"}`, 2)
	var mu sync.Mutex
	counts := map[string]int{}
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		counts[req.Header.Get("Authorization")]++
		mu.Unlock()
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := chatRequest(t, h, body, nil); rec.Code != http.StatusOK {
				t.Errorf("status=%d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if counts["Bearer one"] != 5 || counts["Bearer two"] != 5 {
		t.Errorf("round-robin counts=%v", counts)
	}
}

func TestNativeChatComboFallback(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false, "comboStrategy": "fallback"})
	seedProviderChatConn(t, db, "groq-1", "groq", `{"apiKey":"groq-key"}`, 1)
	seedProviderChatConn(t, db, "cohere-1", "cohere", `{"apiKey":"cohere-key"}`, 1)
	seedChatCombo(t, db, "fast-code", []string{"groq/llama-3.3-70b-versatile", "cohere/command-r-08-2024"})
	var calls []string
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Header.Get("Authorization"))
		if req.Header.Get("Authorization") == "Bearer groq-key" {
			return response(http.StatusTooManyRequests, "application/json", `{"error":{"message":"rate limit"}}`), nil
		}
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	h, proxied, _ := chatHarness(db, doer)
	body := `{"model":"fast-code","stream":false,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusOK || *proxied != 0 {
		t.Fatalf("status=%d proxied=%d body=%s", rec.Code, *proxied, rec.Body.String())
	}
	if strings.Join(calls, ",") != "Bearer groq-key,Bearer cohere-key" {
		t.Errorf("combo order=%v", calls)
	}
}

func TestNativeChatComboNoFallbackOn400(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedProviderChatConn(t, db, "groq-1", "groq", `{"apiKey":"groq-key"}`, 1)
	seedProviderChatConn(t, db, "cohere-1", "cohere", `{"apiKey":"cohere-key"}`, 1)
	seedChatCombo(t, db, "strict-code", []string{"groq/llama-3.3-70b-versatile", "cohere/command-r-08-2024"})
	calls := 0
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(http.StatusBadRequest, "application/json", `{"error":{"message":"bad input"}}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"strict-code","stream":false,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestNativeChatComboRoundRobinSticky(t *testing.T) {
	comboRotations.Lock()
	comboRotations.byName = map[string]comboRotation{}
	comboRotations.Unlock()
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false, "comboStrategy": "round-robin", "comboStickyRoundRobinLimit": 2})
	seedProviderChatConn(t, db, "groq-1", "groq", `{"apiKey":"groq-key"}`, 1)
	seedProviderChatConn(t, db, "cohere-1", "cohere", `{"apiKey":"cohere-key"}`, 1)
	seedChatCombo(t, db, "rotating-code", []string{"groq/llama-3.3-70b-versatile", "cohere/command-r-08-2024"})
	var calls []string
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Header.Get("Authorization"))
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"rotating-code","stream":false,"messages":[{"role":"user","content":"x"}]}`
	for range 3 {
		if rec := chatRequest(t, h, body, nil); rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
	}
	if strings.Join(calls, ",") != "Bearer groq-key,Bearer groq-key,Bearer cohere-key" {
		t.Errorf("combo sticky order=%v", calls)
	}
}

func TestNativeChatComboUnsupportedProxiesWithoutRotation(t *testing.T) {
	comboRotations.Lock()
	comboRotations.byName = map[string]comboRotation{}
	comboRotations.Unlock()
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false, "comboStrategy": "round-robin"})
	seedProviderChatConn(t, db, "groq-1", "groq", `{"apiKey":"groq-key"}`, 1)
	seedChatCombo(t, db, "mixed-code", []string{"groq/llama-3.3-70b-versatile", "unknown/model"})
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("native upstream called")
		return nil, nil
	})
	h, proxied, proxiedBody := chatHarness(db, doer)
	body := `{"model":"mixed-code","stream":false,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusOK || *proxied != 1 || *proxiedBody != body {
		t.Fatalf("status=%d proxied=%d", rec.Code, *proxied)
	}
	comboRotations.Lock()
	state := comboRotations.byName["mixed-code"]
	comboRotations.Unlock()
	if state != (comboRotation{}) {
		t.Errorf("ineligible combo advanced rotation: %+v", state)
	}
}

func TestNativeChatComboFusionProxies(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false, "comboStrategy": "fusion"})
	seedProviderChatConn(t, db, "groq-1", "groq", `{"apiKey":"groq-key"}`, 1)
	seedChatCombo(t, db, "fusion-code", []string{"groq/llama-3.3-70b-versatile"})
	h, proxied, _ := chatHarness(db, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("native upstream called")
		return nil, nil
	}))
	body := `{"model":"fusion-code","stream":false,"messages":[{"role":"user","content":"x"}]}`
	if rec := chatRequest(t, h, body, nil); rec.Code != http.StatusOK || *proxied != 1 {
		t.Fatalf("status=%d proxied=%d", rec.Code, *proxied)
	}
}

func TestNativeChatNetworkFailureLocksModel(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream"}`, 1)
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d retry=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	var raw string
	_ = db.QueryRow("SELECT data FROM providerConnections WHERE id='conn-1'").Scan(&raw)
	var data map[string]any
	_ = json.Unmarshal([]byte(raw), &data)
	if data["modelLock_llama-3.3-70b-versatile"] == nil || data["testStatus"] != "unavailable" {
		t.Errorf("connection state=%v", data)
	}
}

func TestNativeChat429EscalatesBackoff(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream","backoffLevel":2}`, 1)
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusTooManyRequests, "application/json", `{"error":{"message":"rate limit"}}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d retry=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	var raw string
	_ = db.QueryRow("SELECT data FROM providerConnections WHERE id='conn-1'").Scan(&raw)
	var data map[string]any
	_ = json.Unmarshal([]byte(raw), &data)
	if data["backoffLevel"] != float64(3) || data["modelLock_llama-3.3-70b-versatile"] == nil {
		t.Errorf("connection state=%v", data)
	}
}

func TestNativeChatSSENormalizesAndEstimatesUsage(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream"}`, 1)
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "text/event-stream", "data: {\"id\":\"chat\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[]},\"finish_reason\":null}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	out := rec.Body.String()
	if strings.Count(out, "data: [DONE]") != 1 || strings.Contains(out, `"tool_calls":[]`) || !strings.Contains(out, `"estimated":true`) {
		t.Errorf("stream normalization failed: %s", out)
	}
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) || !strings.Contains(out, `"id":"chatcmpl-`) {
		t.Errorf("required fields not normalized: %s", out)
	}
	var count int
	var prompt, completion int64
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(MAX(promptTokens),0), COALESCE(MAX(completionTokens),0) FROM usageHistory").Scan(&count, &prompt, &completion); err != nil {
		t.Fatal(err)
	}
	if count != 1 || prompt == 0 || completion == 0 {
		t.Errorf("stored estimated usage count=%d prompt=%d completion=%d", count, prompt, completion)
	}
}

func TestNativeChatSSEEstimatesWithoutFinishChunk(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream"}`, 1)
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"+
			"data: [DONE]\n\n"), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var count int
	var prompt, completion int64
	_ = db.QueryRow("SELECT COUNT(*), COALESCE(MAX(promptTokens),0), COALESCE(MAX(completionTokens),0) FROM usageHistory").Scan(&count, &prompt, &completion)
	if count != 1 || prompt == 0 || completion == 0 {
		t.Errorf("stored estimated usage count=%d prompt=%d completion=%d", count, prompt, completion)
	}
}

func TestNativeChatObservabilityEnvDisablesDetails(t *testing.T) {
	t.Setenv("OBSERVABILITY_ENABLED", "false")
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream"}`, 1)
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"messages":[{"role":"user","content":"x"}]}`
	if rec := chatRequest(t, h, body, nil); rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM requestDetails").Scan(&count)
	if count != 0 {
		t.Errorf("requestDetails count=%d, want 0", count)
	}
}

func TestNativeChatRejectsNonSSEContentType(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream"}`, 1)
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "text/html", `<html><title>secret upstream page</title></html>`), nil
	})
	h, _, _ := chatHarness(db, doer)
	body := `{"model":"groq/llama-3.3-70b-versatile","stream":true,"messages":[{"role":"user","content":"x"}]}`
	rec := chatRequest(t, h, body, nil)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "secret upstream page") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNativeChatConcurrentUsageWrites(t *testing.T) {
	db := openChatDB(t)
	seedChatSettings(t, db, map[string]any{"requireApiKey": false, "enableObservability2": false})
	seedChatConn(t, db, "conn-1", `{"apiKey":"upstream"}`, 1)
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var requestBody map[string]any
		_ = json.NewDecoder(req.Body).Decode(&requestBody)
		n, _ := strconv.Atoi(requestBody["user"].(string))
		return response(http.StatusOK, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":`+strconv.Itoa(10+n)+`,"completion_tokens":2}}`), nil
	})
	h, _, _ := chatHarness(db, doer)
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"model":"groq/llama-3.3-70b-versatile","stream":false,"user":"` + strconv.Itoa(i) + `","messages":[{"role":"user","content":"x"}]}`
			rec := chatRequest(t, h, body, nil)
			if rec.Code != http.StatusOK {
				t.Errorf("status=%d", rec.Code)
			}
		}(i)
	}
	wg.Wait()
	deadline := time.Now().Add(time.Second)
	var count int
	for time.Now().Before(deadline) {
		_ = db.QueryRow("SELECT COUNT(*) FROM usageHistory").Scan(&count)
		if count == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count != 5 {
		t.Errorf("usage count=%d, want 5", count)
	}
}
