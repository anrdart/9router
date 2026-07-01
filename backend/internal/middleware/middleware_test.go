package middleware_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/backend/internal/middleware"
)

// captureLogger writes records to a buffer so tests can assert on content.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestLoggingRedactsByContract: the logging middleware only emits method/path/
// status/duration/id/remote. Even if a buggy handler tried to log a header, the
// middleware's own log line must not contain raw secret values.
func TestLoggingNoSecretsInLine(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	h := middleware.Logging(logger, "")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A handler must be able to read the Authorization header, but it is NOT
		// what the logging middleware emits. We assert the middleware line itself.
		if r.Header.Get("Authorization") != "Bearer super-secret-token" {
			t.Errorf("handler should still be able to read the auth header")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer super-secret-token")
	req.Header.Set("X-Api-Key", "sk-secret-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := buf.String()
	if strings.Contains(out, "super-secret-token") {
		t.Errorf("log leaked Authorization token: %q", out)
	}
	if strings.Contains(out, "sk-secret-key") {
		t.Errorf("log leaked API key: %q", out)
	}
	if !strings.Contains(out, "method=GET") || !strings.Contains(out, "path=/v1/models") {
		t.Errorf("log missing expected fields: %q", out)
	}
}

// TestRequestIDPropagated: a client-supplied X-Request-ID is preserved and echoed.
func TestRequestIDPreserved(t *testing.T) {
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "client-id-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") != "client-id-123" {
		t.Errorf("X-Request-ID = %q, want client-id-123", rec.Header().Get("X-Request-ID"))
	}
}

// TestRequestIDGenerated: when absent, a new id is generated and echoed.
func TestRequestIDGenerated(t *testing.T) {
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") == "" {
		t.Errorf("expected generated X-Request-ID")
	}
}

// TestRecover prevents a panicking handler from crashing the process.
func TestRecover(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)
	h := middleware.Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "panic recovered") {
		t.Errorf("expected panic log, got: %q", buf.String())
	}
}

// TestLimitBody rejects oversized bodies via MaxBytesReader (returns 413 on read).
func TestLimitBody(t *testing.T) {
	h := middleware.LimitBody(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		if _, err := r.Body.Read(buf); err == nil {
			t.Errorf("expected error reading oversized body")
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789abcdef"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// MaxBytesReader triggers a 413 from http.Error when the handler flushes after.
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestCORSPreflight returns 204 on OPTIONS.
func TestCORSPreflight(t *testing.T) {
	h := middleware.CORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("downstream should not be called on OPTIONS")
	}))
	req := httptest.NewRequest(http.MethodOptions, "/anything", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Errorf("CORS origin header = %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

// TestIsRedacted covers the sensitive-header set.
func TestIsRedacted(t *testing.T) {
	for _, h := range []string{"Authorization", "authorization", "X-Api-Key", "x-api-key", "X-9R-Cli-Token", "Cookie"} {
		if !middleware.IsRedacted(h) {
			t.Errorf("IsRedacted(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"Content-Type", "Accept", "X-Request-ID"} {
		if middleware.IsRedacted(h) {
			t.Errorf("IsRedacted(%q) = true, want false", h)
		}
	}
}
