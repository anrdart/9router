package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"9router/backend/internal/config"
	"9router/backend/internal/database"
)

func TestChatRouteUsesNativeHandler(t *testing.T) {
	proxied := false
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		w.WriteHeader(http.StatusTeapot)
	}))
	defer node.Close()

	dataDir := t.TempDir()
	db, err := database.Open(context.Background(), dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(config.Config{
		Host: "127.0.0.1", Port: 0, NodeUpstream: node.URL, DataDir: dataDir,
		RequestBodyMaxBytes: 1 << 20, ReadTimeout: time.Second, WriteTimeout: time.Second,
	}, db, logger)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || proxied {
		t.Fatalf("status=%d proxied=%v body=%s", rec.Code, proxied, rec.Body.String())
	}
}
