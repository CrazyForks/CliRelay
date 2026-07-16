package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// APIKeyDailySpendingReset is a per-key baseline for project-timezone daily spending.
// Effective today cost = max(raw_today_cost - cost_baseline, 0) when day_key matches today.
type APIKeyDailySpendingReset struct {
	TenantID     string
	APIKeyID     string
	DayKey       string
	CostBaseline float64
	ResetAt      time.Time
}

// TIMESTAMP works on both SQLite (affinity) and PostgreSQL (native type).
// Avoid DATETIME: PostgreSQL rejects it when bootstrap runs on a PG connection.
const apiKeyDailySpendingResetsTableSQL = `
CREATE TABLE IF NOT EXISTS api_key_daily_spending_resets (
  tenant_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  api_key_id    TEXT NOT NULL,
  day_key       TEXT NOT NULL DEFAULT '',
  cost_baseline REAL NOT NULL DEFAULT 0,
  reset_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (tenant_id, api_key_id)
);
CREATE INDEX IF NOT EXISTS idx_api_key_daily_spending_resets_day
  ON api_key_daily_spending_resets(tenant_id, day_key);
`

func ensureAPIKeyDailySpendingResetsTable(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(apiKeyDailySpendingResetsTableSQL); err != nil {
		return fmt.Errorf("usage: ensure api_key_daily_spending_resets: %w", err)
	}
	return nil
}

func bootstrapAPIKeyDailySpendingResets(db *sql.DB) error {
	return ensureAPIKeyDailySpendingResetsTable(db)
}

// QueryRawTodayCostByKeyForTenant returns SUM(cost) for the current project day (no reset baseline).
func QueryRawTodayCostByKeyForTenant(tenantID, apiKey string) (float64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	tenantID = normalizeTenantID(tenantID)
	clause, args := buildSingleAPIKeySelectorClauseForTenant(tenantID, apiKey)
	if clause == "" {
		return 0, nil
	}
	queryArgs := append([]interface{}{tenantID}, args...)
	queryArgs = append(queryArgs, CutoffStartUTC(1).Format(time.RFC3339))
	predicate := strings.Replace(clause, " WHERE ", " AND ", 1)
	var total float64
	err := db.QueryRow(
		"SELECT COALESCE(SUM(cost), 0) FROM request_logs WHERE tenant_id = ?"+predicate+" AND timestamp >= ?",
		queryArgs...,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("usage: query raw today cost: %w", err)
	}
	return total, nil
}

// QueryTodayCostByKey returns effective project-day cost after applying any same-day reset baseline.
func QueryTodayCostByKey(apiKey string) (float64, error) {
	tenantID := ResolveAPIKeyTenant(apiKey)
	if tenantID == "" {
		tenantID = systemTenantID
	}
	return QueryTodayEffectiveCostByKeyForTenant(tenantID, apiKey)
}

// QueryTodayEffectiveCostByKeyForTenant returns max(raw_today - baseline, 0) for the key.
func QueryTodayEffectiveCostByKeyForTenant(tenantID, apiKey string) (float64, error) {
	raw, err := QueryRawTodayCostByKeyForTenant(tenantID, apiKey)
	if err != nil {
		return 0, err
	}
	row := GetAPIKeyForTenant(tenantID, apiKey)
	if row == nil || strings.TrimSpace(row.ID) == "" {
		return raw, nil
	}
	baseline, ok, err := GetDailySpendingResetBaseline(tenantID, row.ID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return raw, nil
	}
	return effectiveTodayCost(raw, baseline), nil
}

func effectiveTodayCost(raw, baseline float64) float64 {
	used := raw - baseline
	if used < 0 {
		return 0
	}
	return used
}

// DailySpendingRemaining returns remaining budget for a limit, or nil when unlimited (limit <= 0).
func DailySpendingRemaining(limit, used float64) *float64 {
	if limit <= 0 {
		return nil
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return &remaining
}

// GetDailySpendingResetBaseline returns today's cost baseline when a same-day reset exists.
func GetDailySpendingResetBaseline(tenantID, apiKeyID string) (float64, bool, error) {
	db := getDB()
	if db == nil {
		return 0, false, nil
	}
	tenantID = normalizeTenantID(tenantID)
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return 0, false, nil
	}
	var dayKey string
	var baseline float64
	err := db.QueryRow(
		`SELECT day_key, cost_baseline FROM api_key_daily_spending_resets WHERE tenant_id = ? AND api_key_id = ?`,
		tenantID, apiKeyID,
	).Scan(&dayKey, &baseline)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("usage: get daily spending reset: %w", err)
	}
	if strings.TrimSpace(dayKey) != LocalDayKeyAt(time.Now()) {
		return 0, false, nil
	}
	return baseline, true, nil
}

// ListDailySpendingResetBaselines returns same-day baselines keyed by api_key_id.
func ListDailySpendingResetBaselines(tenantID string, apiKeyIDs []string) (map[string]float64, error) {
	out := make(map[string]float64)
	if len(apiKeyIDs) == 0 {
		return out, nil
	}
	db := getDB()
	if db == nil {
		return out, nil
	}
	tenantID = normalizeTenantID(tenantID)
	today := LocalDayKeyAt(time.Now())
	ids := make([]string, 0, len(apiKeyIDs))
	seen := make(map[string]struct{}, len(apiKeyIDs))
	for _, id := range apiKeyIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)+2)
	args = append(args, tenantID, today)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT api_key_id, cost_baseline FROM api_key_daily_spending_resets
		 WHERE tenant_id = ? AND day_key = ? AND api_key_id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: list daily spending resets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var baseline float64
		if err := rows.Scan(&id, &baseline); err != nil {
			return nil, fmt.Errorf("usage: scan daily spending reset: %w", err)
		}
		out[strings.TrimSpace(id)] = baseline
	}
	return out, rows.Err()
}

// QueryRawTodayCostsByKeysForTenant batch-loads raw today costs for many keys (by id/key).
// Result is keyed by api_key_id when present, else by api key string.
func QueryRawTodayCostsByKeysForTenant(tenantID string, keys []APIKeyRow) (map[string]float64, error) {
	out := make(map[string]float64)
	if len(keys) == 0 {
		return out, nil
	}
	db := getDB()
	if db == nil {
		return out, nil
	}
	tenantID = normalizeTenantID(tenantID)
	ids := make([]string, 0, len(keys))
	keyStrings := make([]string, 0, len(keys))
	idSet := make(map[string]struct{}, len(keys))
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if id := strings.TrimSpace(k.ID); id != "" {
			if _, ok := idSet[id]; !ok {
				idSet[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		if key := strings.TrimSpace(k.Key); key != "" {
			if _, ok := keySet[key]; !ok {
				keySet[key] = struct{}{}
				keyStrings = append(keyStrings, key)
			}
		}
	}
	if len(ids) == 0 && len(keyStrings) == 0 {
		return out, nil
	}

	var b strings.Builder
	args := make([]interface{}, 0, 2+len(ids)+len(keyStrings))
	b.WriteString(`SELECT api_key_id, api_key, COALESCE(SUM(cost), 0) FROM request_logs WHERE tenant_id = ? AND timestamp >= ? AND (`)
	args = append(args, tenantID, CutoffStartUTC(1).Format(time.RFC3339))
	parts := make([]string, 0, 2)
	if len(ids) > 0 {
		ph := make([]string, len(ids))
		for i, id := range ids {
			ph[i] = "?"
			args = append(args, id)
		}
		parts = append(parts, `api_key_id IN (`+strings.Join(ph, ",")+`)`)
	}
	if len(keyStrings) > 0 {
		ph := make([]string, len(keyStrings))
		for i, key := range keyStrings {
			ph[i] = "?"
			args = append(args, key)
		}
		parts = append(parts, `(api_key_id = '' AND api_key IN (`+strings.Join(ph, ",")+`))`)
	}
	b.WriteString(strings.Join(parts, " OR "))
	b.WriteString(`) GROUP BY api_key_id, api_key`)

	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: batch raw today cost: %w", err)
	}
	defer rows.Close()

	// Map raw rows into lookup by id and by key string for callers.
	costByID := make(map[string]float64)
	costByKey := make(map[string]float64)
	// legacyCostByKey: only api_key_id='' rows (matches single-key selector OR branch).
	legacyCostByKey := make(map[string]float64)
	for rows.Next() {
		var apiKeyID, apiKey string
		var cost float64
		if err := rows.Scan(&apiKeyID, &apiKey, &cost); err != nil {
			return nil, fmt.Errorf("usage: scan batch raw today cost: %w", err)
		}
		apiKeyID = strings.TrimSpace(apiKeyID)
		apiKey = strings.TrimSpace(apiKey)
		if apiKeyID != "" {
			costByID[apiKeyID] += cost
			continue
		}
		if apiKey != "" {
			legacyCostByKey[apiKey] += cost
			costByKey[apiKey] += cost
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Match buildSingleAPIKeySelectorClauseForTenant:
	// (api_key_id = ? OR (api_key_id = '' AND api_key = ?)) — sum both branches.
	for _, k := range keys {
		id := strings.TrimSpace(k.ID)
		key := strings.TrimSpace(k.Key)
		if id != "" {
			out[id] = costByID[id] + legacyCostByKey[key]
			continue
		}
		if key != "" {
			out[key] = costByKey[key]
		}
	}
	return out, nil
}

// UpsertDailySpendingReset stores/replaces the same-day baseline for a key.
func UpsertDailySpendingReset(tenantID, apiKeyID string, costBaseline float64) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	tenantID = normalizeTenantID(tenantID)
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return fmt.Errorf("usage: api_key_id is required")
	}
	if costBaseline < 0 {
		costBaseline = 0
	}
	now := time.Now()
	dayKey := LocalDayKeyAt(now)
	_, err := db.Exec(
		`INSERT INTO api_key_daily_spending_resets (tenant_id, api_key_id, day_key, cost_baseline, reset_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, api_key_id) DO UPDATE SET
		   day_key = excluded.day_key,
		   cost_baseline = excluded.cost_baseline,
		   reset_at = excluded.reset_at`,
		tenantID, apiKeyID, dayKey, costBaseline, now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("usage: upsert daily spending reset: %w", err)
	}
	return nil
}

// DeleteDailySpendingReset removes the reset marker for a key (e.g. on key delete).
func DeleteDailySpendingReset(tenantID, apiKeyID string) error {
	db := getDB()
	if db == nil {
		return nil
	}
	tenantID = normalizeTenantID(tenantID)
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return nil
	}
	_, err := db.Exec(
		`DELETE FROM api_key_daily_spending_resets WHERE tenant_id = ? AND api_key_id = ?`,
		tenantID, apiKeyID,
	)
	if err != nil {
		return fmt.Errorf("usage: delete daily spending reset: %w", err)
	}
	return nil
}
