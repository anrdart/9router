package database

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// tableDef mirrors one entry of TABLES in src/lib/db/schema.js.
type tableDef struct {
	name       string
	columns    []columnDef // ordered; appended if missing
	primaryKey string      // appended after columns if non-empty
	indexes    []indexDef  // created IF NOT EXISTS
}

type columnDef struct {
	name string
	def  string // e.g. "TEXT PRIMARY KEY", "TEXT NOT NULL"
}

type indexDef struct {
	name string
	stmt string // full CREATE INDEX IF NOT EXISTS ... statement
}

// Tables is the declarative current schema, kept in sync with
// src/lib/db/schema.js TABLES. SyncSchema creates missing tables/columns/
// indexes idempotently — it never drops or alters existing columns (destructive
// changes require a versioned migration, added to both Node and Go together).
var Tables = []tableDef{
	{
		name: "_meta",
		columns: []columnDef{
			{"key", "TEXT PRIMARY KEY"},
			{"value", "TEXT NOT NULL"},
		},
	},
	{
		name: "settings",
		columns: []columnDef{
			{"id", "INTEGER PRIMARY KEY CHECK (id = 1)"},
			{"data", "TEXT NOT NULL"},
		},
	},
	{
		name: "providerConnections",
		columns: []columnDef{
			{"id", "TEXT PRIMARY KEY"},
			{"provider", "TEXT NOT NULL"},
			{"authType", "TEXT NOT NULL"},
			{"name", "TEXT"},
			{"email", "TEXT"},
			{"priority", "INTEGER"},
			{"isActive", "INTEGER DEFAULT 1"},
			{"data", "TEXT NOT NULL"},
			{"createdAt", "TEXT NOT NULL"},
			{"updatedAt", "TEXT NOT NULL"},
		},
		indexes: []indexDef{
			{"idx_pc_provider", "CREATE INDEX IF NOT EXISTS idx_pc_provider ON providerConnections(provider)"},
			{"idx_pc_provider_active", "CREATE INDEX IF NOT EXISTS idx_pc_provider_active ON providerConnections(provider, isActive)"},
			{"idx_pc_priority", "CREATE INDEX IF NOT EXISTS idx_pc_priority ON providerConnections(priority)"},
		},
	},
	{
		name: "providerNodes",
		columns: []columnDef{
			{"id", "TEXT PRIMARY KEY"},
			{"type", "TEXT"},
			{"name", "TEXT"},
			{"data", "TEXT NOT NULL"},
			{"createdAt", "TEXT NOT NULL"},
			{"updatedAt", "TEXT NOT NULL"},
		},
		indexes: []indexDef{
			{"idx_pn_type", "CREATE INDEX IF NOT EXISTS idx_pn_type ON providerNodes(type)"},
		},
	},
	{
		name: "proxyPools",
		columns: []columnDef{
			{"id", "TEXT PRIMARY KEY"},
			{"isActive", "INTEGER DEFAULT 1"},
			{"testStatus", "TEXT"},
			{"data", "TEXT NOT NULL"},
			{"createdAt", "TEXT NOT NULL"},
			{"updatedAt", "TEXT NOT NULL"},
		},
		indexes: []indexDef{
			{"idx_pp_active", "CREATE INDEX IF NOT EXISTS idx_pp_active ON proxyPools(isActive)"},
			{"idx_pp_status", "CREATE INDEX IF NOT EXISTS idx_pp_status ON proxyPools(testStatus)"},
		},
	},
	{
		name: "apiKeys",
		columns: []columnDef{
			{"id", "TEXT PRIMARY KEY"},
			{"key", "TEXT UNIQUE NOT NULL"},
			{"name", "TEXT"},
			{"machineId", "TEXT"},
			{"isActive", "INTEGER DEFAULT 1"},
			{"createdAt", "TEXT NOT NULL"},
		},
		indexes: []indexDef{
			{"idx_ak_key", "CREATE INDEX IF NOT EXISTS idx_ak_key ON apiKeys(key)"},
		},
	},
	{
		name: "combos",
		columns: []columnDef{
			{"id", "TEXT PRIMARY KEY"},
			{"name", "TEXT UNIQUE NOT NULL"},
			{"kind", "TEXT"},
			{"models", "TEXT NOT NULL"},
			{"createdAt", "TEXT NOT NULL"},
			{"updatedAt", "TEXT NOT NULL"},
		},
		indexes: []indexDef{
			{"idx_combo_name", "CREATE INDEX IF NOT EXISTS idx_combo_name ON combos(name)"},
		},
	},
	{
		name: "kv",
		columns: []columnDef{
			{"scope", "TEXT NOT NULL"},
			{"key", "TEXT NOT NULL"},
			{"value", "TEXT NOT NULL"},
		},
		primaryKey: "PRIMARY KEY (scope, key)",
		indexes: []indexDef{
			{"idx_kv_scope", "CREATE INDEX IF NOT EXISTS idx_kv_scope ON kv(scope)"},
		},
	},
	{
		name: "usageHistory",
		columns: []columnDef{
			{"id", "INTEGER PRIMARY KEY AUTOINCREMENT"},
			{"timestamp", "TEXT NOT NULL"},
			{"provider", "TEXT"},
			{"model", "TEXT"},
			{"connectionId", "TEXT"},
			{"apiKey", "TEXT"},
			{"endpoint", "TEXT"},
			{"promptTokens", "INTEGER DEFAULT 0"},
			{"completionTokens", "INTEGER DEFAULT 0"},
			{"cost", "REAL DEFAULT 0"},
			{"status", "TEXT"},
			{"tokens", "TEXT"},
			{"meta", "TEXT"},
		},
		indexes: []indexDef{
			{"idx_uh_ts", "CREATE INDEX IF NOT EXISTS idx_uh_ts ON usageHistory(timestamp DESC)"},
			{"idx_uh_provider", "CREATE INDEX IF NOT EXISTS idx_uh_provider ON usageHistory(provider)"},
			{"idx_uh_model", "CREATE INDEX IF NOT EXISTS idx_uh_model ON usageHistory(model)"},
			{"idx_uh_conn", "CREATE INDEX IF NOT EXISTS idx_uh_conn ON usageHistory(connectionId)"},
		},
	},
	{
		name: "usageDaily",
		columns: []columnDef{
			{"dateKey", "TEXT PRIMARY KEY"},
			{"data", "TEXT NOT NULL"},
		},
	},
	{
		name: "requestDetails",
		columns: []columnDef{
			{"id", "TEXT PRIMARY KEY"},
			{"timestamp", "TEXT NOT NULL"},
			{"provider", "TEXT"},
			{"model", "TEXT"},
			{"connectionId", "TEXT"},
			{"status", "TEXT"},
			{"data", "TEXT NOT NULL"},
		},
		indexes: []indexDef{
			{"idx_rd_ts", "CREATE INDEX IF NOT EXISTS idx_rd_ts ON requestDetails(timestamp DESC)"},
			{"idx_rd_provider", "CREATE INDEX IF NOT EXISTS idx_rd_provider ON requestDetails(provider)"},
			{"idx_rd_model", "CREATE INDEX IF NOT EXISTS idx_rd_model ON requestDetails(model)"},
			{"idx_rd_conn", "CREATE INDEX IF NOT EXISTS idx_rd_conn ON requestDetails(connectionId)"},
		},
	},
}

// buildCreateTableSQL mirrors buildCreateTableSql() in src/lib/db/schema.js.
func buildCreateTableSQL(t tableDef) string {
	parts := make([]string, 0, len(t.columns)+1)
	for _, c := range t.columns {
		parts = append(parts, c.name+" "+c.def)
	}
	if t.primaryKey != "" {
		parts = append(parts, t.primaryKey)
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", t.name, strings.Join(parts, ", "))
}

// SyncSchema creates any missing tables and indexes, and adds any missing
// columns to existing tables (ADD COLUMN). It is idempotent and never drops
// anything. This mirrors syncSchemaFromTables() in src/lib/db/schema.js.
func (db *DB) SyncSchema(ctx context.Context, logger *slog.Logger) error {
	for _, t := range Tables {
		if _, err := db.ExecContext(ctx, buildCreateTableSQL(t)); err != nil {
			return fmt.Errorf("create table %s: %w", t.name, err)
		}

		// Add missing columns to existing tables (non-destructive).
		existing, err := db.columnsOf(ctx, t.name)
		if err != nil {
			return fmt.Errorf("introspect %s: %w", t.name, err)
		}
		for _, c := range t.columns {
			if _, ok := existing[c.name]; ok {
				continue
			}
			// A PRIMARY KEY/CHECK column can't be added via ALTER TABLE; skip those
			// (they only exist on a freshly created table anyway).
			if strings.Contains(strings.ToUpper(c.def), "PRIMARY KEY") || strings.Contains(strings.ToUpper(c.def), "CHECK") {
				continue
			}
			alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", t.name, c.name, c.def)
			if _, err := db.ExecContext(ctx, alter); err != nil {
				// Concurrent SyncSchema from Node can race on ADD COLUMN; a duplicate
				// add is benign. Log and continue.
				logger.Warn("add column failed (may already exist)",
					slog.String("table", t.name), slog.String("column", c.name),
					slog.String("err", err.Error()))
			}
		}

		for _, ix := range t.indexes {
			if _, err := db.ExecContext(ctx, ix.stmt); err != nil {
				return fmt.Errorf("create index %s: %w", ix.name, err)
			}
		}
	}
	return nil
}

// columnsOf returns the set of column names present in a table (for ADD COLUMN checks).
func (db *DB) columnsOf(ctx context.Context, table string) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}
