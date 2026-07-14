// Package server wires the Go-backend HTTP server: routing, middleware, and the
// reverse proxy fallback to the Node engine.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"9router/backend/internal/config"
	"9router/backend/internal/database"
	"9router/backend/internal/httpapi"
	"9router/backend/internal/middleware"
	"9router/backend/internal/proxy"
)

// New builds the configured *http.Server. The caller owns its lifecycle
// (Serve/Shutdown).
//
// Routing strategy (Phase 2): the default for every path is the reverse proxy.
// Native handlers are registered for specific METHOD+PATH pairs. When a request
// arrives, we first check if Go has a native handler for that exact method+path;
// if yes, it runs (behind auth); if not, the request falls through to the proxy
// so Node handles it unchanged. This means mutations (PATCH/POST/DELETE) to a
// path that Go only serves as GET are still proxied to Node automatically.
func New(cfg config.Config, db *database.DB, logger *slog.Logger) (*http.Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	rp, err := proxy.Reverse(cfg.NodeUpstream, logger)
	if err != nil {
		return nil, err
	}

	// Build a single mux that serves both native routes and the catch-all proxy.
	// Go 1.22+ ServeMux supports method-pattern routing ("GET /path"), and a bare
	// "/" pattern catches everything not matched by a more specific pattern.
	mux := http.NewServeMux()

	// --- Public native route ------------------------------------------------
	mux.HandleFunc("GET /health", httpapi.Health)

	// GET /v1/models is public (the Node route has no auth gate) and exposes only
	// model ids — no connection data or secrets. It serves the embedded static
	// catalog + DB combos/custom/aliases natively, and reverse-proxies to Node
	// only when an active connection needs live/network model resolution.
	mux.Handle("GET /v1/models", httpapi.NewModelsHandler(rp, logger))

	// Native only for the generated, fail-closed OpenAI-compatible slice. The
	// handler replays the original body to Node whenever translation, multiple
	// accounts, OAuth refresh, proxy transport, or request transforms are needed.
	mux.Handle("POST /v1/chat/completions", httpapi.NewChatHandler(rp, logger))

	// --- Protected /api/* reads --------------------------------------------
	// Each is registered with an exact method so a different method (e.g. PATCH
	// /api/settings) does NOT match and falls through to "/" → proxy → Node.
	// The guard reads Node's shared secrets under DATA_DIR to accept dashboard
	// JWT sessions / CLI tokens without a proxy round trip.
	guard := middleware.NewGuard(cfg.DataDir)
	mux.Handle("GET /api/settings", guard.Require(http.HandlerFunc(httpapi.SettingsGET)))
	mux.Handle("GET /api/keys", guard.Require(http.HandlerFunc(httpapi.APIKeysGET)))
	mux.Handle("GET /api/providers", guard.Require(http.HandlerFunc(httpapi.ProvidersGET)))
	mux.Handle("GET /api/usage/logs", guard.Require(http.HandlerFunc(httpapi.UsageLogsGET)))
	mux.Handle("GET /api/usage/stats", guard.Require(http.HandlerFunc(httpapi.UsageStatsGET)))

	// --- Catch-all reverse proxy (everything else) --------------------------
	mux.Handle("/", rp)

	// Outermost -> innermost: recover, DB-injection, request-id, logging,
	// body-limit, CORS.
	handler := middleware.Chain(mux,
		middleware.Recover(logger),
		middleware.WithDB(db),
		middleware.RequestID,
		middleware.Logging(logger, ""),
		middleware.LimitBody(cfg.RequestBodyMaxBytes),
		middleware.CORS,
	)

	return &http.Server{
		Addr:              cfg.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}, nil
}

// Run starts the server and blocks until ctx is cancelled or Serve errors.
func Run(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down", slog.Duration("timeout", shutdownTimeout))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("err", err.Error()))
			return err
		}
		logger.Info("shutdown complete")
		return nil
	case err := <-errCh:
		return err
	}
}
