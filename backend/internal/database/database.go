// Package database manages the 9Router SQLite store.
//
// It opens the SAME database file the Node engine uses (DATA_DIR/db/data.sqlite),
// applies the same PRAGMAs (WAL, busy_timeout, …) so both processes can coexist,
// and auto-syncs missing tables/columns to mirror src/lib/db/schema.js.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// DB wraps a *sql.DB connection to the 9Router SQLite store plus a write mutex.
// SQLite serializes writes well, but we keep a mutex so Go-side transactions
// never contend with each other (matching Node's single-writer adapter pattern).
type DB struct {
	*sql.DB
	mu sync.Mutex
}

// Path resolves the SQLite file path using the same logic as src/lib/dataDir.js +
// src/lib/db/paths.js: DATA_DIR/db/data.sqlite. When DATA_DIR is unset it falls
// back to ~/.9router. The parent directories are created if missing.
func Path(dataDir string) (string, error) {
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		dataDir = filepath.Join(home, ".9router")
	}
	dbDir := filepath.Join(dataDir, "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return "", fmt.Errorf("create db dir %s: %w", dbDir, err)
	}
	return filepath.Join(dbDir, "data.sqlite"), nil
}

// Open connects to the SQLite file and configures connection-level PRAGMAs.
// The DSN pragmas mirror src/lib/db/schema.js (WAL, synchronous=NORMAL,
// busy_timeout, foreign_keys) so Go and Node interoperate safely. A nil logger
// is replaced with a Discard handler so callers in tests need not supply one.
func Open(ctx context.Context, dataDir string, logger *slog.Logger) (*DB, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	dbPath, err := Path(dataDir)
	if err != nil {
		return nil, err
	}

	// Build a DSN with query pragmas (modernc.org/sqlite reads these).
	// busy_timeout=5000 matches Node's PRAGMA busy_timeout.
	q := url.Values{}
	q.Set("_pragma", "journal_mode(WAL)")
	q.Set("_pragma", "synchronous(NORMAL)")
	q.Set("_pragma", "temp_store(MEMORY)")
	q.Set("_pragma", "mmap_size(30000000)")
	q.Set("_pragma", "cache_size(-64000)")
	q.Set("_pragma", "foreign_keys(on)")
	q.Set("_pragma", "busy_timeout(5000)")
	dsn := "file:" + dbPath + "?" + q.Encode()

	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}

	// SQLite is most reliable as a single connection for writers. Set a modest
	// connection pool so concurrent reads do not block; the mutex serializes writes.
	sdb.SetMaxOpenConns(1)

	if err := sdb.PingContext(ctx); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	logger.Info("database opened", slog.String("path", dbPath))

	db := &DB{DB: sdb}
	if err := db.SyncSchema(ctx, logger); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

// WithWriteLock acquires the write mutex then runs fn with the context.
// Use for any transactional write so Go-side writers never deadlock each other.
func (db *DB) WithWriteLock(ctx context.Context, fn func(tx *sql.Tx) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
