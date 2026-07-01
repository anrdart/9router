package proxy_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"9router/backend/internal/proxy"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestReverseForwards verifies a plain GET is forwarded with method/path/body
// preserved and the upstream response is returned verbatim.
func TestReverseForwards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("upstream method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"hello":"world"}`)
	}))
	defer upstream.Close()

	rp, err := proxy.Reverse(upstream.URL, silentLogger())
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Errorf("upstream header missing")
	}
	if strings.TrimSpace(rec.Body.String()) != `{"hello":"world"}` {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestReverseStreaming verifies chunked SSE-like responses are streamed through
// (not buffered until completion).
func TestReverseStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream writer is not a flusher")
		}
		_, _ = io.WriteString(w, "data: chunk1\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: chunk2\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	rp, err := proxy.Reverse(upstream.URL, silentLogger())
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "chunk1") || !strings.Contains(body, "[DONE]") {
		t.Errorf("streamed body missing chunks: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// TestReverseClientDisconnect verifies that when the client context is cancelled,
// the upstream handler observes the cancellation (no goroutine/connection leak).
func TestReverseClientDisconnect(t *testing.T) {
	upstreamCancelled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a long-lived stream that watches the request context.
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
		upstreamCancelled <- struct{}{}
	}))
	defer upstream.Close()

	rp, err := proxy.Reverse(upstream.URL, silentLogger())
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		rp.ServeHTTP(rec, req)
		close(done)
	}()

	// Give the upstream a moment to start streaming, then disconnect the client.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-upstreamCancelled:
		// good: upstream saw the cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not observe client disconnect within timeout")
	}
	<-done
}

// TestReverseBadUpstream returns 502 when the upstream is unreachable.
func TestReverseBadUpstream(t *testing.T) {
	rp, err := proxy.Reverse("http://127.0.0.1:1", silentLogger())
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	rp.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// TestReverseInvalidURL errors on a malformed upstream URL.
func TestReverseInvalidURL(t *testing.T) {
	if _, err := proxy.Reverse("://bad", silentLogger()); err == nil {
		t.Fatal("expected error for malformed upstream URL")
	}
}
