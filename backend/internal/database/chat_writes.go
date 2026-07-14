package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// ChatUsage is the canonical OpenAI-shaped token record written by the native
// chat path. Prompt includes cached/cache-creation subsets, matching Node.
type ChatUsage struct {
	Prompt        int64 `json:"prompt_tokens"`
	Completion    int64 `json:"completion_tokens"`
	Total         int64 `json:"total_tokens"`
	Cached        int64 `json:"cached_tokens,omitempty"`
	Reasoning     int64 `json:"reasoning_tokens,omitempty"`
	CacheCreation int64 `json:"cache_creation_input_tokens,omitempty"`
}

type ChatUsageEntry struct {
	Timestamp    string
	Provider     string
	Model        string
	ConnectionID string
	APIKey       string
	Endpoint     string
	Status       string
	Cost         float64
	Tokens       ChatUsage
}

// SaveChatUsage mirrors saveRequestUsage(): history, daily aggregate, and the
// lifetime counter commit atomically. Duplicate stream completion callbacks are
// suppressed by the same composite equality check Node uses.
func (db *DB) SaveChatUsage(ctx context.Context, e ChatUsageEntry) error {
	if e.Timestamp == "" {
		e.Timestamp = nowISO()
	}
	if e.Status == "" {
		e.Status = "ok"
	}
	if e.Tokens.Total == 0 {
		e.Tokens.Total = e.Tokens.Prompt + e.Tokens.Completion
	}
	tokensJSON, err := json.Marshal(e.Tokens)
	if err != nil {
		return err
	}

	return db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		var id int64
		var endpoint sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, endpoint FROM usageHistory
			WHERE timestamp = ?
			AND COALESCE(provider, '') = COALESCE(?, '')
			AND COALESCE(model, '') = COALESCE(?, '')
			AND COALESCE(connectionId, '') = COALESCE(?, '')
			AND COALESCE(apiKey, '') = COALESCE(?, '')
			AND promptTokens = ? AND completionTokens = ?
			ORDER BY id DESC LIMIT 1`,
			e.Timestamp, nullString(e.Provider), nullString(e.Model), nullString(e.ConnectionID), nullString(e.APIKey), e.Tokens.Prompt, e.Tokens.Completion,
		).Scan(&id, &endpoint)
		if err == nil {
			if !endpoint.Valid && e.Endpoint != "" {
				_, err = tx.ExecContext(ctx, "UPDATE usageHistory SET endpoint = ? WHERE id = ?", e.Endpoint, id)
			}
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO usageHistory
			(timestamp, provider, model, connectionId, apiKey, endpoint, promptTokens, completionTokens, cost, status, tokens, meta)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}')`,
			e.Timestamp, nullString(e.Provider), nullString(e.Model), nullString(e.ConnectionID), nullString(e.APIKey), nullString(e.Endpoint),
			e.Tokens.Prompt, e.Tokens.Completion, e.Cost, e.Status, string(tokensJSON)); err != nil {
			return err
		}

		dateKey := localDateKey(e.Timestamp)
		day := newUsageDay()
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT data FROM usageDaily WHERE dateKey = ?", dateKey).Scan(&raw); err == nil {
			_ = json.Unmarshal([]byte(raw), &day)
		} else if err != sql.ErrNoRows {
			return err
		}
		aggregateChatUsage(day, e)
		dayJSON, err := json.Marshal(day)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usageDaily(dateKey, data) VALUES(?, ?)
			ON CONFLICT(dateKey) DO UPDATE SET data = excluded.data`, dateKey, string(dayJSON)); err != nil {
			return err
		}

		var current string
		_ = tx.QueryRowContext(ctx, "SELECT value FROM _meta WHERE key = 'totalRequestsLifetime'").Scan(&current)
		n, _ := strconv.ParseInt(current, 10, 64)
		_, err = tx.ExecContext(ctx, `INSERT INTO _meta(key, value) VALUES('totalRequestsLifetime', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(n+1, 10))
		return err
	})
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newUsageDay() map[string]any {
	return map[string]any{
		"requests": float64(0), "promptTokens": float64(0), "completionTokens": float64(0), "cachedTokens": float64(0), "cost": float64(0),
		"byProvider": map[string]any{}, "byModel": map[string]any{}, "byAccount": map[string]any{}, "byApiKey": map[string]any{}, "byEndpoint": map[string]any{},
	}
}

func aggregateChatUsage(day map[string]any, e ChatUsageEntry) {
	addNumber(day, "requests", 1)
	addNumber(day, "promptTokens", float64(e.Tokens.Prompt))
	addNumber(day, "completionTokens", float64(e.Tokens.Completion))
	addNumber(day, "cachedTokens", float64(e.Tokens.Cached))
	addNumber(day, "cost", e.Cost)
	vals := map[string]any{
		"requests": float64(1), "promptTokens": float64(e.Tokens.Prompt), "completionTokens": float64(e.Tokens.Completion),
		"cachedTokens": float64(e.Tokens.Cached), "cost": e.Cost,
	}
	if e.Provider != "" {
		addUsageBucket(day, "byProvider", e.Provider, vals, nil)
	}
	modelKey := e.Model
	if e.Provider != "" {
		modelKey = e.Model + "|" + e.Provider
	}
	addUsageBucket(day, "byModel", modelKey, vals, map[string]any{"rawModel": e.Model, "provider": e.Provider})
	if e.ConnectionID != "" {
		addUsageBucket(day, "byAccount", e.ConnectionID, vals, map[string]any{"rawModel": e.Model, "provider": e.Provider})
	}
	apiKey := e.APIKey
	if apiKey == "" {
		apiKey = "local-no-key"
	}
	addUsageBucket(day, "byApiKey", fmt.Sprintf("%s|%s|%s", apiKey, e.Model, fallbackString(e.Provider, "unknown")), vals,
		map[string]any{"rawModel": e.Model, "provider": e.Provider, "apiKey": nullString(e.APIKey)})
	endpoint := fallbackString(e.Endpoint, "Unknown")
	addUsageBucket(day, "byEndpoint", fmt.Sprintf("%s|%s|%s", endpoint, e.Model, fallbackString(e.Provider, "unknown")), vals,
		map[string]any{"endpoint": endpoint, "rawModel": e.Model, "provider": e.Provider})
}

func addNumber(m map[string]any, key string, delta float64) {
	cur, _ := m[key].(float64)
	m[key] = cur + delta
}

func addUsageBucket(day map[string]any, section, key string, vals, meta map[string]any) {
	obj, _ := day[section].(map[string]any)
	if obj == nil {
		obj = map[string]any{}
		day[section] = obj
	}
	bucket, _ := obj[key].(map[string]any)
	if bucket == nil {
		bucket = map[string]any{}
		obj[key] = bucket
	}
	for _, field := range []string{"requests", "promptTokens", "completionTokens", "cachedTokens", "cost"} {
		addNumber(bucket, field, number(vals[field]))
	}
	for k, v := range meta {
		bucket[k] = v
	}
}

func number(v any) float64 {
	n, _ := v.(float64)
	return n
}

func fallbackString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// UpdateConnectionSelectionState atomically merges round-robin bookkeeping into
// one connection's opaque data blob.
func (db *DB) UpdateConnectionSelectionState(ctx context.Context, id, lastUsedAt string, consecutiveUseCount int) error {
	return db.UpdateConnectionChatState(ctx, id, map[string]any{
		"lastUsedAt": lastUsedAt, "consecutiveUseCount": consecutiveUseCount,
	})
}

// UpdateConnectionChatState merges native fallback state into the opaque data
// JSON without touching credentials or other provider-specific fields.
func (db *DB) UpdateConnectionChatState(ctx context.Context, id string, updates map[string]any) error {
	if id == "" || len(updates) == 0 {
		return nil
	}
	return db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT data FROM providerConnections WHERE id = ?", id).Scan(&raw); err != nil {
			return err
		}
		data := map[string]any{}
		_ = json.Unmarshal([]byte(raw), &data)
		for k, v := range updates {
			if v == nil {
				delete(data, k)
			} else {
				data[k] = v
			}
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE providerConnections SET data = ?, updatedAt = ? WHERE id = ?", string(encoded), nowISO(), id)
		return err
	})
}

// ClearConnectionModelError clears the succeeded model lock plus expired locks.
// Error status resets only when no active model lock remains, matching Node.
func (db *DB) ClearConnectionModelError(ctx context.Context, id, model string, now time.Time) error {
	if id == "" {
		return nil
	}
	return db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT data FROM providerConnections WHERE id = ?", id).Scan(&raw); err != nil {
			return err
		}
		data := map[string]any{}
		_ = json.Unmarshal([]byte(raw), &data)
		for key, value := range data {
			if len(key) < len("modelLock_") || key[:len("modelLock_")] != "modelLock_" {
				continue
			}
			expiry, _ := value.(string)
			t, _ := time.Parse(time.RFC3339Nano, expiry)
			if key == "modelLock_"+model || key == "modelLock___all" || (!t.IsZero() && !t.After(now)) {
				delete(data, key)
			}
		}
		active := false
		for key, value := range data {
			if len(key) < len("modelLock_") || key[:len("modelLock_")] != "modelLock_" {
				continue
			}
			expiry, _ := value.(string)
			if t, err := time.Parse(time.RFC3339Nano, expiry); err == nil && t.After(now) {
				active = true
				break
			}
		}
		if !active {
			data["testStatus"] = "active"
			delete(data, "lastError")
			delete(data, "lastErrorAt")
			data["backoffLevel"] = float64(0)
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE providerConnections SET data = ?, updatedAt = ? WHERE id = ?", string(encoded), nowISO(), id)
		return err
	})
}

// SaveChatRequestDetail writes one sanitized observability record. The native
// handler never includes inbound headers, so stored data cannot contain auth.
func (db *DB) SaveChatRequestDetail(ctx context.Context, id, provider, model, connectionID, status string, data map[string]any, maxRecords int) error {
	if maxRecords <= 0 {
		maxRecords = 200
	}
	if id == "" {
		id = fmt.Sprintf("%s-%d", model, time.Now().UnixNano())
	}
	timestamp := nowISO()
	data["id"] = id
	data["timestamp"] = timestamp
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return db.WithWriteLock(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO requestDetails(id, timestamp, provider, model, connectionId, status, data)
			VALUES(?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET timestamp=excluded.timestamp, provider=excluded.provider, model=excluded.model,
			connectionId=excluded.connectionId, status=excluded.status, data=excluded.data`,
			id, timestamp, nullString(provider), nullString(model), nullString(connectionID), nullString(status), string(raw)); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM requestDetails").Scan(&count); err != nil {
			return err
		}
		if count > maxRecords {
			_, err := tx.ExecContext(ctx, `DELETE FROM requestDetails WHERE id IN
				(SELECT id FROM requestDetails ORDER BY timestamp ASC LIMIT ?)`, count-maxRecords)
			return err
		}
		return nil
	})
}
