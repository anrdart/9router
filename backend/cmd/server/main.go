// Command server is the 9router Go backend entry point.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"9router/backend/internal/config"
	"9router/backend/internal/database"
	"9router/backend/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", slog.String("err", err.Error()))
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)

	logger.Info("9router go backend starting",
		slog.Int("port", cfg.Port),
		slog.String("upstream", cfg.NodeUpstream),
		slog.String("data_dir", cfg.DataDir),
	)

	// Open the SQLite store (same file Node uses). Failure to open the DB is
	// non-fatal: the reverse proxy + /health still work, and native /api routes
	// return a clear error until the DB is available.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, dbErr := database.Open(ctx, cfg.DataDir, logger)
	if dbErr != nil {
		// Log but do not exit: the proxy bridge is still useful without a DB.
		logger.Error("database open failed (native /api routes disabled; proxy still works)",
			slog.String("err", dbErr.Error()))
	}

	srv, err := server.New(cfg, db, logger)
	if err != nil {
		logger.Error("server build failed", slog.String("err", err.Error()))
		os.Exit(1)
	}

	if err := server.Run(ctx, srv, cfg.ShutdownTimeout, logger); err != nil {
		logger.Error("server exited with error", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// newLogger returns a structured JSON logger at the requested level.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
