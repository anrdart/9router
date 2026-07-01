package database

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// rowScanner abstracts *sql.Row and *sql.Rows for scan helpers.
type rowScanner interface {
	Scan(dest ...any) error
}

// ---- Settings ---------------------------------------------------------------

// DefaultSettings mirrors DEFAULT_SETTINGS in src/lib/db/repos/settingsRepo.js.
// Kept as a JSON-friendly map so Go never needs to model every field; the merge
// behaviour (raw over defaults) is identical to mergeWithDefaults().
func DefaultSettings() map[string]any {
	return map[string]any{
		"cloudEnabled":               false,
		"tunnelEnabled":              false,
		"tunnelUrl":                  "",
		"tunnelProvider":             "cloudflare",
		"tailscaleEnabled":           false,
		"tailscaleUrl":               "",
		"stickyRoundRobinLimit":      3,
		"providerStrategies":         map[string]any{},
		"comboStrategy":              "fallback",
		"comboStickyRoundRobinLimit": 1,
		"comboStrategies":            map[string]any{},
		"requireLogin":               true,
		"requireApiKey":              true,
		"tunnelDashboardAccess":      true,
		"authMode":                   "password",
		"oidcIssuerUrl":              "",
		"oidcClientId":               "",
		"oidcScopes":                 "openid profile email",
		"oidcLoginLabel":             "Sign in with OIDC",
		"enableObservability":        true,
		"observabilityMaxRecords":    1000,
		"outboundProxyEnabled":       false,
		"outboundProxyUrl":           "",
		"outboundNoProxy":            "",
		"rtkEnabled":                 true,
		"headroomEnabled":            false,
		"cavemanEnabled":             false,
		"cavemanLevel":               "full",
		"ponytailEnabled":            false,
		"ponytailLevel":              "full",
	}
}

// GetSettings reads the settings JSON blob and merges it with defaults, exactly
// like getSettings() in settingsRepo.js.
func (db *DB) GetSettings(ctx context.Context) (map[string]any, error) {
	var data string
	err := db.QueryRowContext(ctx, "SELECT data FROM settings WHERE id = 1").Scan(&data)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("read settings: %w", err)
	}

	result := DefaultSettings()
	if err != sql.ErrNoRows && data != "" {
		raw := map[string]any{}
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			return nil, fmt.Errorf("parse settings json: %w", err)
		}
		for k, v := range raw {
			result[k] = v
		}
	}
	return result, nil
}

// ---- API keys ---------------------------------------------------------------

// APIKey mirrors rowToKey() in src/lib/db/repos/apiKeysRepo.js.
type APIKey struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Name      string `json:"name"`
	MachineID string `json:"machineId"`
	IsActive  bool   `json:"isActive"`
	CreatedAt string `json:"createdAt"`
}

func scanAPIKey(s rowScanner) (APIKey, error) {
	var k APIKey
	var isActive int
	var name, machineID sql.NullString
	if err := s.Scan(&k.ID, &k.Key, &name, &machineID, &isActive, &k.CreatedAt); err != nil {
		return k, err
	}
	k.Name = name.String
	k.MachineID = machineID.String
	k.IsActive = isActive == 1
	return k, nil
}

// ListAPIKeys returns all keys ordered by createdAt (matches Node ordering).
func (db *DB) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, key, name, machineId, isActive, createdAt FROM apiKeys ORDER BY createdAt ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ValidateApiKey checks a candidate key against active keys in constant time,
// mirroring validateApiKey() in apiKeysRepo.js. Returns false if no keys exist.
func (db *DB) ValidateApiKey(ctx context.Context, candidate string) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT key FROM apiKeys WHERE isActive = 1")
	if err != nil {
		return false, err
	}
	defer rows.Close()

	matched := false
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return false, err
		}
		// Constant-time compare: accumulate so every key does the same work.
		if subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1 {
			matched = true
		}
	}
	return matched, rows.Err()
}

// ---- Provider connections ---------------------------------------------------

// ProviderConnection mirrors the row shape used by exportDb() in index.js.
type ProviderConnection struct {
	ID        string         `json:"id"`
	Provider  string         `json:"provider"`
	AuthType  string         `json:"authType"`
	Name      sql.NullString `json:"-"`
	Email     sql.NullString `json:"-"`
	Priority  sql.NullInt64  `json:"-"`
	IsActive  int            `json:"-"`
	Data      string         `json:"-"`
	CreatedAt string         `json:"createdAt"`
	UpdatedAt string         `json:"updatedAt"`
}

// ToJSON builds the API response object: base fields plus the parsed `data` blob.
// Matches the shape produced by exportDb() in src/lib/db/index.js.
func (c ProviderConnection) ToJSON() (map[string]any, error) {
	base := map[string]any{
		"id":        c.ID,
		"provider":  c.Provider,
		"authType":  c.AuthType,
		"name":      jsonString(c.Name),
		"email":     jsonString(c.Email),
		"priority":  jsonInt(c.Priority),
		"isActive":  c.IsActive == 1,
		"createdAt": c.CreatedAt,
		"updatedAt": c.UpdatedAt,
	}
	// Merge the opaque `data` JSON blob on top, then force the base fields back
	// (the blob can contain stale copies of the same keys).
	var extra map[string]any
	if c.Data != "" {
		_ = json.Unmarshal([]byte(c.Data), &extra)
	}
	merged := map[string]any{}
	for k, v := range extra {
		merged[k] = v
	}
	for k, v := range base {
		merged[k] = v
	}
	return merged, nil
}

func jsonString(n sql.NullString) any {
	if !n.Valid || n.String == "" {
		return nil
	}
	return n.String
}

func jsonInt(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// ListProviderConnections returns connections, optionally filtered by provider
// and/or isActive. Matches getProviderConnections() semantics.
func (db *DB) ListProviderConnections(ctx context.Context, provider string, activeOnly bool) ([]ProviderConnection, error) {
	q := "SELECT id, provider, authType, name, email, priority, isActive, data, createdAt, updatedAt FROM providerConnections"
	var (
		args    []any
		clauses []string
	)
	if provider != "" {
		clauses = append(clauses, "provider = ?")
		args = append(args, provider)
	}
	if activeOnly {
		clauses = append(clauses, "isActive = 1")
	}
	if len(clauses) > 0 {
		q += " WHERE " + joinAnd(clauses)
	}
	q += " ORDER BY provider ASC, priority ASC NULLS LAST, createdAt ASC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderConnection
	for rows.Next() {
		var c ProviderConnection
		if err := rows.Scan(&c.ID, &c.Provider, &c.AuthType, &c.Name, &c.Email, &c.Priority, &c.IsActive, &c.Data, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}

// ProviderNodeNames returns a map of providerNodes id → name, used to enrich the
// display name of OpenAI/Anthropic-compatible connections (mirrors the
// nodeNameMap built in src/app/api/providers/route.js). A read error yields an
// empty map so provider listing degrades gracefully rather than failing.
func (db *DB) ProviderNodeNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := db.QueryContext(ctx, "SELECT id, name FROM providerNodes")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var name sql.NullString
		if err := rows.Scan(&id, &name); err != nil {
			return out
		}
		if id != "" && name.Valid && name.String != "" {
			out[id] = name.String
		}
	}
	return out
}

// ---- Usage history ----------------------------------------------------------

// UsageRow mirrors a usageHistory row (used for read-only /api/usage/*).
// Usage read paths (getRecentLogs / getUsageStats) live in usage.go. The DB
// layer intentionally exposes NO raw-row accessor that carries apiKey, so a
// caller cannot accidentally leak credentials the way an earlier UsageRow.ToJSON
// did — usage output is either pre-formatted log strings or masked aggregates.

// nowISO returns an RFC3339 timestamp, matching Node's new Date().toISOString().
func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
