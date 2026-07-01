package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file ports the read paths of src/lib/db/repos/usageRepo.js that are
// PURELY database-derived: getRecentLogs and the persisted portion of
// getUsageStats. The live in-memory sections (activeRequests, pending,
// errorProvider) exist only in the Node chat process and are emitted empty here;
// the dashboard's SSE stream (/api/usage/stream, still served by Node) supplies
// those fields and never lets them overwrite this REST aggregation
// (UsageStats.js merges only the live fields). So the split is faithful:
// Go owns the persisted aggregation, Node owns the live overlay.

// ValidStatsPeriods mirrors VALID_PERIODS in src/app/api/usage/stats/route.js.
var ValidStatsPeriods = map[string]bool{
	"today": true, "24h": true, "7d": true, "30d": true, "60d": true, "all": true,
}

const period24hMs = 24 * 60 * 60 * 1000

// ---- helpers ---------------------------------------------------------------

// parseTS parses a stored timestamp. Node stores ISO-8601 UTC strings; we also
// accept date-only strings (used by tests) and fall back to UTC midnight.
func parseTS(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z07:00",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// localDateKey mirrors getLocalDateKey(): YYYY-MM-DD in the process-local zone
// (Node uses new Date(iso).getFullYear()/getMonth()/getDate(), which are local).
func localDateKey(ts string) string {
	t, ok := parseTS(ts)
	if !ok {
		if len(ts) >= 10 {
			return ts[:10]
		}
		return ts
	}
	return t.Local().Format("2006-01-02")
}

// maskApiKey mirrors maskApiKey() in usageRepo.js. Returns "" for the JS null.
func maskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return string(key[0]) + "***"
	}
	return key[:8] + "***"
}

// jsGetNum reads a numeric field from a decoded JSON object as float64. JSON
// numbers decode to float64, so this covers both int-ish and fractional values.
func jsGetNum(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

func jsGetStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// bucket is one aggregation row. requests/tokens are integers; cost is float.
// Extra display fields (rawModel, provider, lastUsed, …) are carried in extra so
// the emitted JSON matches the Node shape exactly.
type bucket struct {
	requests   int64
	prompt     int64
	completion int64
	cost       float64
	extra      map[string]any
}

func (b *bucket) add(requests, prompt, completion int64, cost float64) {
	b.requests += requests
	b.prompt += prompt
	b.completion += completion
	b.cost += cost
}

func (b *bucket) toJSON() map[string]any {
	m := map[string]any{
		"requests":         b.requests,
		"promptTokens":     b.prompt,
		"completionTokens": b.completion,
		"cost":             b.cost,
	}
	for k, v := range b.extra {
		m[k] = v
	}
	return m
}

// bucketSet is an insertion-ordered map of buckets, so JSON output order is
// stable (aids diffing against Node, though JS object order is not guaranteed).
type bucketSet struct {
	order []string
	m     map[string]*bucket
}

func newBucketSet() *bucketSet { return &bucketSet{m: map[string]*bucket{}} }

func (s *bucketSet) get(key string) (*bucket, bool) {
	b, ok := s.m[key]
	return b, ok
}

func (s *bucketSet) ensure(key string, extra map[string]any) *bucket {
	if b, ok := s.m[key]; ok {
		return b
	}
	b := &bucket{extra: extra}
	s.m[key] = b
	s.order = append(s.order, key)
	return b
}

func (s *bucketSet) toJSON() map[string]any {
	out := make(map[string]any, len(s.order))
	for _, k := range s.order {
		out[k] = s.m[k].toJSON()
	}
	return out
}

// ---- RecentLogs (getRecentLogs) --------------------------------------------

// RecentLogs returns pre-formatted log lines, mirroring getRecentLogs(). Each is
// "DD-MM-YYYY HH:mm:ss | model | PROVIDER | account | sent | received | status".
// No API key is included (getRecentLogs never selects or emits it).
func (db *DB) RecentLogs(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}

	// Resolve connection names FIRST: the pool is capped at one open connection
	// (SetMaxOpenConns(1)), so issuing this query while the main rows cursor below
	// is still open would deadlock waiting for a second connection.
	connNames := db.connectionDisplayNames(ctx)

	rows, err := db.QueryContext(ctx,
		"SELECT timestamp, provider, model, connectionId, promptTokens, completionTokens, status, tokens FROM usageHistory ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var ts, provider, model, connID, status, tokensJSON sql.NullString
		var promptTok, completionTok sql.NullInt64
		if err := rows.Scan(&ts, &provider, &model, &connID, &promptTok, &completionTok, &status, &tokensJSON); err != nil {
			return nil, err
		}
		p := "-"
		if provider.Valid && provider.String != "" {
			p = strings.ToUpper(provider.String)
		}
		m := "-"
		if model.Valid && model.String != "" {
			m = model.String
		}
		account := "-"
		if connID.Valid && connID.String != "" {
			if n := connNames[connID.String]; n != "" {
				account = n
			} else if len(connID.String) >= 8 {
				account = connID.String[:8]
			} else {
				account = connID.String
			}
		}
		tok := decodeTokens(tokensJSON)
		sent := tokenField(promptTok, tok, "prompt_tokens")
		received := tokenField(completionTok, tok, "completion_tokens")
		dateStr := "-"
		if ts.Valid {
			dateStr = formatLogDate(ts.String)
		}
		st := "-"
		if status.Valid && status.String != "" {
			st = status.String
		}
		out = append(out, fmt.Sprintf("%s | %s | %s | %s | %s | %s | %s",
			dateStr, m, p, account, sent, received, st))
	}
	return out, rows.Err()
}

// tokenField mirrors `col ?? tokens[jsonKey] ?? "-"` (nullish coalescing: 0 is
// kept). Returns a string because the log line renders "-" for missing values.
func tokenField(col sql.NullInt64, tok map[string]any, jsonKey string) string {
	if col.Valid {
		return fmt.Sprintf("%d", col.Int64)
	}
	if v, ok := tok[jsonKey]; ok {
		if f, ok := v.(float64); ok {
			return fmt.Sprintf("%d", int64(f))
		}
	}
	return "-"
}

// formatLogDate mirrors formatLogDate(): DD-MM-YYYY HH:mm:ss in local time.
func formatLogDate(ts string) string {
	t, ok := parseTS(ts)
	if !ok {
		return ts
	}
	return t.Local().Format("02-01-2006 15:04:05")
}

func decodeTokens(s sql.NullString) map[string]any {
	if !s.Valid || s.String == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s.String), &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// connectionDisplayNames maps connection id → name||email||id (getRecentLogs uses
// name||email; getUsageStats uses name||email||id — we key on id fallback here to
// serve both, and RecentLogs falls back to the id slice itself when empty).
func (db *DB) connectionDisplayNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := db.QueryContext(ctx, "SELECT id, name, email FROM providerConnections")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var name, email sql.NullString
		if err := rows.Scan(&id, &name, &email); err != nil {
			return out
		}
		switch {
		case name.Valid && name.String != "":
			out[id] = name.String
		case email.Valid && email.String != "":
			out[id] = email.String
		}
	}
	return out
}

// ---- UsageStats (getUsageStats, DB-only) -----------------------------------

// apiKeyInfo mirrors the apiKeyMap value {name,id,createdAt}.
type apiKeyInfo struct{ name string }

// UsageStats ports the persisted portion of getUsageStats(period). Live fields
// (activeRequests, pending, errorProvider) are emitted empty — see file header.
// now is injected for deterministic tests (pass time.Now()).
func (db *DB) UsageStats(ctx context.Context, period string, now time.Time) (map[string]any, error) {
	connNames := db.connectionStatsNames(ctx) // id → name||email||id
	nodeNames := db.ProviderNodeNames(ctx)    // provider id → display name
	apiKeys := db.apiKeyInfoMap(ctx)          // key → {name}

	stats := map[string]any{
		"totalRequests": int64(0), "totalPromptTokens": int64(0),
		"totalCompletionTokens": int64(0), "totalCost": float64(0),
	}
	byProvider := newBucketSet()
	byModel := newBucketSet()
	byAccount := newBucketSet()
	byAPIKey := newBucketSet()
	byEndpoint := newBucketSet()

	var totalPrompt, totalCompletion int64
	var totalCost float64

	recentRequests, err := db.recentRequests(ctx)
	if err != nil {
		return nil, err
	}

	useDaily := period != "24h" && period != "today"
	if useDaily {
		maxDays := map[string]int{"7d": 7, "30d": 30, "60d": 60}[period] // 0 for "all"
		dayRows, err := db.loadDaysInRange(ctx, maxDays, now)
		if err != nil {
			return nil, err
		}
		for _, dr := range dayRows {
			var day map[string]any
			if json.Unmarshal([]byte(dr.data), &day) != nil || day == nil {
				continue
			}
			dateKey := dr.dateKey
			totalPrompt += int64(jsGetNum(day, "promptTokens"))
			totalCompletion += int64(jsGetNum(day, "completionTokens"))
			totalCost += jsGetNum(day, "cost")

			aggregateDay(day, dateKey, connNames, nodeNames, apiKeys,
				byProvider, byModel, byAccount, byAPIKey, byEndpoint)
		}
		db.overlayLastUsed(ctx, maxDays, now, connNames, byModel, byAccount, byAPIKey, byEndpoint)
	} else {
		cutoff := now.Add(-time.Duration(period24hMs) * time.Millisecond)
		if period == "today" {
			y, m, d := now.Local().Date()
			cutoff = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		}
		if err := db.aggregateHistory(ctx, cutoff, connNames, nodeNames, apiKeys,
			byProvider, byModel, byAccount, byAPIKey, byEndpoint,
			&totalPrompt, &totalCompletion, &totalCost); err != nil {
			return nil, err
		}
	}

	last10, err := db.last10Minutes(ctx, now)
	if err != nil {
		return nil, err
	}

	// totalRequests = sum of byProvider.requests (matches Node).
	var totalReq int64
	for _, k := range byProvider.order {
		totalReq += byProvider.m[k].requests
	}

	stats["totalRequests"] = totalReq
	stats["totalPromptTokens"] = totalPrompt
	stats["totalCompletionTokens"] = totalCompletion
	stats["totalCost"] = totalCost
	stats["byProvider"] = byProvider.toJSON()
	stats["byModel"] = byModel.toJSON()
	stats["byAccount"] = byAccount.toJSON()
	stats["byApiKey"] = byAPIKey.toJSON()
	stats["byEndpoint"] = byEndpoint.toJSON()
	stats["last10Minutes"] = last10
	stats["recentRequests"] = recentRequests
	// Live fields: owned by Node's SSE stream, empty here.
	stats["pending"] = map[string]any{"byModel": map[string]any{}, "byAccount": map[string]any{}}
	stats["activeRequests"] = []any{}
	stats["errorProvider"] = ""

	return stats, nil
}

// aggregateDay folds one usageDaily blob into the running bucket sets, mirroring
// the daily-summary branch of getUsageStats.
func aggregateDay(day map[string]any, dateKey string, connNames, nodeNames map[string]string, apiKeys map[string]apiKeyInfo,
	byProvider, byModel, byAccount, byAPIKey, byEndpoint *bucketSet) {

	iterObj(day["byProvider"], func(prov string, p map[string]any) {
		b := byProvider.ensure(prov, map[string]any{})
		b.add(int64(jsGetNum(p, "requests")), int64(jsGetNum(p, "promptTokens")), int64(jsGetNum(p, "completionTokens")), jsGetNum(p, "cost"))
	})

	iterObj(day["byModel"], func(mk string, m map[string]any) {
		rawModel := firstNonEmpty(jsGetStr(m, "rawModel"), splitIdx(mk, "|", 0))
		provider := firstNonEmpty(jsGetStr(m, "provider"), splitIdx(mk, "|", 1))
		statsKey := rawModel
		if provider != "" {
			statsKey = fmt.Sprintf("%s (%s)", rawModel, provider)
		}
		b := byModel.ensure(statsKey, map[string]any{
			"rawModel": rawModel, "provider": displayName(nodeNames, provider), "lastUsed": dateKey,
		})
		b.add(int64(jsGetNum(m, "requests")), int64(jsGetNum(m, "promptTokens")), int64(jsGetNum(m, "completionTokens")), jsGetNum(m, "cost"))
		if dateKey > lastUsed(b) {
			b.extra["lastUsed"] = dateKey
		}
	})

	iterObj(day["byAccount"], func(connID string, a map[string]any) {
		accountName := accountDisplay(connNames, connID)
		rawModel := jsGetStr(a, "rawModel")
		provider := jsGetStr(a, "provider")
		accountKey := fmt.Sprintf("%s (%s - %s)", rawModel, provider, accountName)
		b := byAccount.ensure(accountKey, map[string]any{
			"rawModel": rawModel, "provider": displayName(nodeNames, provider),
			"connectionId": connID, "accountName": accountName, "lastUsed": dateKey,
		})
		b.add(int64(jsGetNum(a, "requests")), int64(jsGetNum(a, "promptTokens")), int64(jsGetNum(a, "completionTokens")), jsGetNum(a, "cost"))
		if dateKey > lastUsed(b) {
			b.extra["lastUsed"] = dateKey
		}
	})

	iterObj(day["byApiKey"], func(akKey string, ak map[string]any) {
		rawModel := jsGetStr(ak, "rawModel")
		provider := jsGetStr(ak, "provider")
		apiKeyVal := jsGetStr(ak, "apiKey")
		keyName := "Local (No API Key)"
		if apiKeyVal != "" {
			if info, ok := apiKeys[apiKeyVal]; ok && info.name != "" {
				keyName = info.name
			} else if len(apiKeyVal) >= 8 {
				keyName = apiKeyVal[:8] + "..."
			} else {
				keyName = apiKeyVal + "..."
			}
		}
		masked := maskApiKey(apiKeyVal)
		apiKeyKey := masked
		if apiKeyKey == "" {
			apiKeyKey = "local-no-key"
		}
		b := byAPIKey.ensure(akKey, map[string]any{
			"rawModel": rawModel, "provider": displayName(nodeNames, provider),
			"apiKeyMasked": maskedOrNil(masked), "keyName": keyName, "apiKeyKey": apiKeyKey, "lastUsed": dateKey,
		})
		b.add(int64(jsGetNum(ak, "requests")), int64(jsGetNum(ak, "promptTokens")), int64(jsGetNum(ak, "completionTokens")), jsGetNum(ak, "cost"))
		if dateKey > lastUsed(b) {
			b.extra["lastUsed"] = dateKey
		}
	})

	iterObj(day["byEndpoint"], func(epKey string, ep map[string]any) {
		endpoint := firstNonEmpty(jsGetStr(ep, "endpoint"), splitIdx(epKey, "|", 0), "Unknown")
		rawModel := jsGetStr(ep, "rawModel")
		provider := jsGetStr(ep, "provider")
		b := byEndpoint.ensure(epKey, map[string]any{
			"endpoint": endpoint, "rawModel": rawModel, "provider": displayName(nodeNames, provider), "lastUsed": dateKey,
		})
		b.add(int64(jsGetNum(ep, "requests")), int64(jsGetNum(ep, "promptTokens")), int64(jsGetNum(ep, "completionTokens")), jsGetNum(ep, "cost"))
		if dateKey > lastUsed(b) {
			b.extra["lastUsed"] = dateKey
		}
	})
}

// aggregateHistory folds raw usageHistory rows since cutoff into the bucket sets,
// mirroring the 24h/today branch of getUsageStats (one increment per row).
func (db *DB) aggregateHistory(ctx context.Context, cutoff time.Time,
	connNames, nodeNames map[string]string, apiKeys map[string]apiKeyInfo,
	byProvider, byModel, byAccount, byAPIKey, byEndpoint *bucketSet,
	totalPrompt, totalCompletion *int64, totalCost *float64) error {

	rows, err := db.QueryContext(ctx,
		"SELECT timestamp, provider, model, connectionId, apiKey, endpoint, promptTokens, completionTokens, cost, tokens FROM usageHistory WHERE timestamp >= ?",
		cutoff.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var ts, provider, model, connID, apiKey, endpoint, tokensJSON sql.NullString
		var promptCol, completionCol sql.NullInt64
		var costCol sql.NullFloat64
		if err := rows.Scan(&ts, &provider, &model, &connID, &apiKey, &endpoint, &promptCol, &completionCol, &costCol, &tokensJSON); err != nil {
			return err
		}
		tok := decodeTokens(tokensJSON)
		prompt := int64(numFromColOrToken(promptCol, tok, "prompt_tokens"))
		completion := int64(numFromColOrToken(completionCol, tok, "completion_tokens"))
		var cost float64
		if costCol.Valid {
			cost = costCol.Float64
		}
		prov := provider.String
		mdl := model.String
		timestamp := ts.String
		provDisplay := displayName(nodeNames, prov)

		*totalPrompt += prompt
		*totalCompletion += completion
		*totalCost += cost

		// byProvider
		bp := byProvider.ensure(prov, map[string]any{})
		bp.add(1, prompt, completion, cost)

		// byModel
		modelKey := mdl
		if prov != "" {
			modelKey = fmt.Sprintf("%s (%s)", mdl, prov)
		}
		bm := byModel.ensure(modelKey, map[string]any{"rawModel": mdl, "provider": provDisplay, "lastUsed": timestamp})
		bm.add(1, prompt, completion, cost)
		if newerTS(timestamp, lastUsed(bm)) {
			bm.extra["lastUsed"] = timestamp
		}

		// byAccount (only when connectionId present)
		if connID.Valid && connID.String != "" {
			accountName := accountDisplay(connNames, connID.String)
			accountKey := fmt.Sprintf("%s (%s - %s)", mdl, prov, accountName)
			ba := byAccount.ensure(accountKey, map[string]any{
				"rawModel": mdl, "provider": provDisplay, "connectionId": connID.String, "accountName": accountName, "lastUsed": timestamp,
			})
			ba.add(1, prompt, completion, cost)
			if newerTS(timestamp, lastUsed(ba)) {
				ba.extra["lastUsed"] = timestamp
			}
		}

		// byApiKey
		if apiKey.Valid && apiKey.String != "" {
			key := apiKey.String
			keyName := key[:min(8, len(key))] + "..."
			if info, ok := apiKeys[key]; ok && info.name != "" {
				keyName = info.name
			}
			masked := maskApiKey(key)
			akKey := fmt.Sprintf("%s|%s|%s", masked, mdl, orUnknown(prov))
			ba := byAPIKey.ensure(akKey, map[string]any{
				"rawModel": mdl, "provider": provDisplay, "apiKeyMasked": maskedOrNil(masked),
				"keyName": keyName, "apiKeyKey": masked, "lastUsed": timestamp,
			})
			ba.add(1, prompt, completion, cost)
			if newerTS(timestamp, lastUsed(ba)) {
				ba.extra["lastUsed"] = timestamp
			}
		} else {
			ba := byAPIKey.ensure("local-no-key", map[string]any{
				"rawModel": mdl, "provider": provDisplay, "apiKeyMasked": nil,
				"keyName": "Local (No API Key)", "apiKeyKey": "local-no-key", "lastUsed": timestamp,
			})
			ba.add(1, prompt, completion, cost)
			if newerTS(timestamp, lastUsed(ba)) {
				ba.extra["lastUsed"] = timestamp
			}
		}

		// byEndpoint
		ep := "Unknown"
		if endpoint.Valid && endpoint.String != "" {
			ep = endpoint.String
		}
		epKey := fmt.Sprintf("%s|%s|%s", ep, mdl, orUnknown(prov))
		be := byEndpoint.ensure(epKey, map[string]any{"endpoint": ep, "rawModel": mdl, "provider": provDisplay, "lastUsed": timestamp})
		be.add(1, prompt, completion, cost)
		if newerTS(timestamp, lastUsed(be)) {
			be.extra["lastUsed"] = timestamp
		}
	}
	return rows.Err()
}

// overlayLastUsed refreshes lastUsed with precise history timestamps, mirroring
// the overlay loop of the daily-summary branch.
func (db *DB) overlayLastUsed(ctx context.Context, maxDays int, now time.Time,
	connNames map[string]string, byModel, byAccount, byAPIKey, byEndpoint *bucketSet) {

	cutoff := time.Unix(0, 0).UTC()
	if maxDays > 0 {
		cutoff = now.Add(-time.Duration(maxDays) * 24 * time.Hour)
	}
	rows, err := db.QueryContext(ctx,
		"SELECT timestamp, provider, model, connectionId, apiKey, endpoint FROM usageHistory WHERE timestamp >= ?",
		cutoff.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ts, provider, model, connID, apiKey, endpoint sql.NullString
		if rows.Scan(&ts, &provider, &model, &connID, &apiKey, &endpoint) != nil {
			return
		}
		t := ts.String
		prov, mdl := provider.String, model.String

		modelKey := mdl
		if prov != "" {
			modelKey = fmt.Sprintf("%s (%s)", mdl, prov)
		}
		if b, ok := byModel.get(modelKey); ok && newerTS(t, lastUsed(b)) {
			b.extra["lastUsed"] = t
		}
		if connID.Valid && connID.String != "" {
			accountName := accountDisplay(connNames, connID.String)
			accountKey := fmt.Sprintf("%s (%s - %s)", mdl, prov, accountName)
			if b, ok := byAccount.get(accountKey); ok && newerTS(t, lastUsed(b)) {
				b.extra["lastUsed"] = t
			}
		}
		akKey := "local-no-key"
		if apiKey.Valid && apiKey.String != "" {
			akKey = fmt.Sprintf("%s|%s|%s", apiKey.String, mdl, orUnknown(prov))
		}
		if b, ok := byAPIKey.get(akKey); ok && newerTS(t, lastUsed(b)) {
			b.extra["lastUsed"] = t
		}
		ep := "Unknown"
		if endpoint.Valid && endpoint.String != "" {
			ep = endpoint.String
		}
		endpointKey := fmt.Sprintf("%s|%s|%s", ep, mdl, orUnknown(prov))
		if b, ok := byEndpoint.get(endpointKey); ok && newerTS(t, lastUsed(b)) {
			b.extra["lastUsed"] = t
		}
	}
}

// recentRequests mirrors the deduped recentRequests list (last 100 rows, keep
// rows with tokens, dedupe by model|provider|prompt|completion|minute, cap 20).
func (db *DB) recentRequests(ctx context.Context) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT timestamp, provider, model, tokens, status FROM usageHistory ORDER BY id DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	seen := map[string]bool{}
	for rows.Next() {
		var ts, provider, model, tokensJSON, status sql.NullString
		if err := rows.Scan(&ts, &provider, &model, &tokensJSON, &status); err != nil {
			return nil, err
		}
		tok := decodeTokens(tokensJSON)
		prompt := int64(firstNum(tok, "prompt_tokens", "input_tokens"))
		completion := int64(firstNum(tok, "completion_tokens", "output_tokens"))
		if prompt == 0 && completion == 0 {
			continue
		}
		minute := ""
		if ts.Valid && len(ts.String) >= 16 {
			minute = ts.String[:16]
		}
		key := fmt.Sprintf("%s|%s|%d|%d|%s", model.String, provider.String, prompt, completion, minute)
		if seen[key] {
			continue
		}
		seen[key] = true
		st := "ok"
		if status.Valid && status.String != "" {
			st = status.String
		}
		out = append(out, map[string]any{
			"timestamp": ts.String, "model": model.String, "provider": provider.String,
			"promptTokens": prompt, "completionTokens": completion, "status": st,
		})
		if len(out) >= 20 {
			break
		}
	}
	return out, rows.Err()
}

// last10Minutes returns 10 one-minute buckets ending at the current minute,
// mirroring the last10Minutes section.
func (db *DB) last10Minutes(ctx context.Context, now time.Time) ([]map[string]any, error) {
	currentMinute := now.Truncate(time.Minute)
	tenAgo := currentMinute.Add(-9 * time.Minute)

	type acc struct {
		requests, prompt, completion int64
		cost                         float64
	}
	buckets := make([]acc, 10)
	starts := make([]int64, 10)
	for i := 0; i < 10; i++ {
		starts[i] = currentMinute.Add(time.Duration(-(9 - i)) * time.Minute).UnixMilli()
	}
	idxOf := func(ms int64) int {
		for i, s := range starts {
			if ms == s {
				return i
			}
		}
		return -1
	}

	rows, err := db.QueryContext(ctx,
		"SELECT timestamp, promptTokens, completionTokens, cost FROM usageHistory WHERE timestamp >= ? AND timestamp <= ?",
		tenAgo.UTC().Format("2006-01-02T15:04:05.000Z"), now.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts sql.NullString
		var prompt, completion sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&ts, &prompt, &completion, &cost); err != nil {
			return nil, err
		}
		t, ok := parseTS(ts.String)
		if !ok {
			continue
		}
		minuteStart := t.Truncate(time.Minute).UnixMilli()
		if i := idxOf(minuteStart); i >= 0 {
			buckets[i].requests++
			if prompt.Valid {
				buckets[i].prompt += prompt.Int64
			}
			if completion.Valid {
				buckets[i].completion += completion.Int64
			}
			if cost.Valid {
				buckets[i].cost += cost.Float64
			}
		}
	}
	out := make([]map[string]any, 10)
	for i, b := range buckets {
		out[i] = map[string]any{"requests": b.requests, "promptTokens": b.prompt, "completionTokens": b.completion, "cost": b.cost}
	}
	return out, nil
}

// dayRow is one usageDaily record.
type dayRow struct {
	dateKey string
	data    string
}

// loadDaysInRange mirrors loadDaysInRange(): all rows when maxDays<=0, else
// rows with dateKey >= cutoff (now - maxDays + 1 day, local).
func (db *DB) loadDaysInRange(ctx context.Context, maxDays int, now time.Time) ([]dayRow, error) {
	var rows *sql.Rows
	var err error
	if maxDays <= 0 {
		rows, err = db.QueryContext(ctx, "SELECT dateKey, data FROM usageDaily")
	} else {
		cutoff := now.Local().AddDate(0, 0, -(maxDays - 1))
		cutoffKey := cutoff.Format("2006-01-02")
		rows, err = db.QueryContext(ctx, "SELECT dateKey, data FROM usageDaily WHERE dateKey >= ?", cutoffKey)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dayRow
	for rows.Next() {
		var d dayRow
		if err := rows.Scan(&d.dateKey, &d.data); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// connectionStatsNames maps id → name||email||id (getUsageStats variant).
func (db *DB) connectionStatsNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := db.QueryContext(ctx, "SELECT id, name, email FROM providerConnections")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var name, email sql.NullString
		if rows.Scan(&id, &name, &email) != nil {
			return out
		}
		switch {
		case name.Valid && name.String != "":
			out[id] = name.String
		case email.Valid && email.String != "":
			out[id] = email.String
		default:
			out[id] = id
		}
	}
	return out
}

func (db *DB) apiKeyInfoMap(ctx context.Context) map[string]apiKeyInfo {
	out := map[string]apiKeyInfo{}
	rows, err := db.QueryContext(ctx, "SELECT key, name FROM apiKeys")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var name sql.NullString
		if rows.Scan(&key, &name) != nil {
			return out
		}
		out[key] = apiKeyInfo{name: name.String}
	}
	return out
}

// ---- small helpers ---------------------------------------------------------

func iterObj(v any, fn func(key string, val map[string]any)) {
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, raw := range obj {
		if m, ok := raw.(map[string]any); ok {
			fn(k, m)
		}
	}
}

func lastUsed(b *bucket) string {
	if s, ok := b.extra["lastUsed"].(string); ok {
		return s
	}
	return ""
}

// newerTS reports whether candidate is a strictly later instant than current.
// Falls back to string compare when either is unparseable (ISO sorts lexically).
func newerTS(candidate, current string) bool {
	ct, cok := parseTS(candidate)
	pt, pok := parseTS(current)
	if cok && pok {
		return ct.After(pt)
	}
	return candidate > current
}

func displayName(nodeNames map[string]string, provider string) string {
	if n := nodeNames[provider]; n != "" {
		return n
	}
	return provider
}

func accountDisplay(connNames map[string]string, connID string) string {
	if n := connNames[connID]; n != "" {
		return n
	}
	return "Account " + connID[:min(8, len(connID))] + "..."
}

func maskedOrNil(masked string) any {
	if masked == "" {
		return nil
	}
	return masked
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func splitIdx(s, sep string, idx int) string {
	parts := strings.Split(s, sep)
	if idx < len(parts) {
		return parts[idx]
	}
	return ""
}

func firstNum(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v := jsGetNum(m, k); v != 0 {
			return v
		}
	}
	return 0
}

func numFromColOrToken(col sql.NullInt64, tok map[string]any, jsonKey string) float64 {
	if col.Valid {
		return float64(col.Int64)
	}
	return jsGetNum(tok, jsonKey)
}
