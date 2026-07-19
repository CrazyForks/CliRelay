package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const endUserDailySpendingResetsTableSQL = `
CREATE TABLE IF NOT EXISTS end_user_daily_spending_resets (
  tenant_id     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  end_user_id   TEXT NOT NULL,
  day_key       TEXT NOT NULL DEFAULT '',
  cost_baseline REAL NOT NULL DEFAULT 0,
  reset_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (tenant_id, end_user_id)
);
CREATE INDEX IF NOT EXISTS idx_end_user_daily_spending_resets_day
  ON end_user_daily_spending_resets(tenant_id, day_key);
`

func bootstrapEndUserDailySpendingResets(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(endUserDailySpendingResetsTableSQL); err != nil {
		return fmt.Errorf("usage: ensure end_user_daily_spending_resets: %w", err)
	}
	return nil
}

func QueryRawTodayCostByEndUserForTenant(tenantID, endUserID string) (float64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	tenantID = normalizeTenantID(tenantID)
	endUserID = strings.TrimSpace(endUserID)
	predicate, args := buildEndUserAPIKeySelectorPredicate(tenantID, endUserID)
	queryArgs := append([]interface{}{tenantID, CutoffStartUTC(1).Format(time.RFC3339)}, args...)
	var total float64
	if err := db.QueryRow(
		"SELECT COALESCE(SUM(cost), 0) FROM request_logs WHERE tenant_id = ? AND timestamp >= ? AND "+predicate,
		queryArgs...,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("usage: query raw today cost by end user: %w", err)
	}
	return total, nil
}

func getEndUserDailySpendingResetBaseline(tenantID, endUserID string) (float64, bool, error) {
	db := getReadDB()
	if db == nil {
		return 0, false, nil
	}
	tenantID = normalizeTenantID(tenantID)
	endUserID = strings.TrimSpace(endUserID)
	var dayKey string
	var baseline float64
	err := db.QueryRow(
		`SELECT day_key, cost_baseline FROM end_user_daily_spending_resets WHERE tenant_id = ? AND end_user_id = ?`,
		tenantID, endUserID,
	).Scan(&dayKey, &baseline)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("usage: get end-user daily spending baseline: %w", err)
	}
	if dayKey != LocalDayKeyAt(time.Now()) {
		return 0, false, nil
	}
	return baseline, true, nil
}

func QueryTodayEffectiveCostByEndUserForTenant(tenantID, endUserID string) (float64, error) {
	raw, err := QueryRawTodayCostByEndUserForTenant(tenantID, endUserID)
	if err != nil {
		return 0, err
	}
	baseline, ok, err := getEndUserDailySpendingResetBaseline(tenantID, endUserID)
	if err != nil || !ok {
		return raw, err
	}
	return effectiveTodayCost(raw, baseline), nil
}

// QueryTodayEffectiveCostsByEndUsersForTenant batch-loads account-level today costs
// for many end users in a few queries (avoids per-row N+1 on GetEndUsers).
func QueryTodayEffectiveCostsByEndUsersForTenant(tenantID string, endUserIDs []string) (map[string]float64, error) {
	out := make(map[string]float64, len(endUserIDs))
	tenantID = normalizeTenantID(tenantID)
	ids := make([]string, 0, len(endUserIDs))
	seen := make(map[string]struct{}, len(endUserIDs))
	for _, id := range endUserIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		out[id] = 0
	}
	if len(ids) == 0 {
		return out, nil
	}
	db := getReadDB()
	if db == nil {
		return out, nil
	}

	// Sum today's cost per end_user via owned keys (stable id or legacy secret).
	// Arg order: join tenant, end_user ids..., log tenant, cutoff.
	ph := make([]string, len(ids))
	queryArgs := make([]interface{}, 0, 3+len(ids))
	queryArgs = append(queryArgs, tenantID)
	for i, id := range ids {
		ph[i] = "?"
		queryArgs = append(queryArgs, id)
	}
	queryArgs = append(queryArgs, tenantID, CutoffStartUTC(1).Format(time.RFC3339))
	rows, err := db.Query(`
		SELECT k.end_user_id, COALESCE(SUM(r.cost), 0)
		FROM request_logs r
		INNER JOIN api_keys k ON k.tenant_id = ?
			AND k.end_user_id IN (`+strings.Join(ph, ",")+`)
			AND (
				(trim(coalesce(r.api_key_id, '')) <> '' AND r.api_key_id = k.id)
				OR (trim(coalesce(r.api_key_id, '')) = '' AND r.api_key = k.key)
			)
		WHERE r.tenant_id = ? AND r.timestamp >= ?
		GROUP BY k.end_user_id
	`, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("usage: batch today cost by end users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var endUserID string
		var total float64
		if err := rows.Scan(&endUserID, &total); err != nil {
			return nil, fmt.Errorf("usage: scan batch today cost: %w", err)
		}
		out[strings.TrimSpace(endUserID)] = total
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Apply same-day account baselines.
	bph := make([]string, len(ids))
	bargs := make([]interface{}, 0, 2+len(ids))
	bargs = append(bargs, tenantID, LocalDayKeyAt(time.Now()))
	for i, id := range ids {
		bph[i] = "?"
		bargs = append(bargs, id)
	}
	brows, err := db.Query(`
		SELECT end_user_id, cost_baseline FROM end_user_daily_spending_resets
		WHERE tenant_id = ? AND day_key = ? AND end_user_id IN (`+strings.Join(bph, ",")+`)
	`, bargs...)
	if err != nil {
		return nil, fmt.Errorf("usage: batch end-user baselines: %w", err)
	}
	defer brows.Close()
	for brows.Next() {
		var endUserID string
		var baseline float64
		if err := brows.Scan(&endUserID, &baseline); err != nil {
			return nil, err
		}
		endUserID = strings.TrimSpace(endUserID)
		out[endUserID] = effectiveTodayCost(out[endUserID], baseline)
	}
	return out, brows.Err()
}

// ResetTodayCostByEndUser sets an account-level baseline without deleting logs.
func ResetTodayCostByEndUser(tenantID, endUserID string) (usedBefore float64, rawToday float64, err error) {
	db := getDB()
	if db == nil {
		return 0, 0, nil
	}
	tenantID = normalizeTenantID(tenantID)
	endUserID = strings.TrimSpace(endUserID)
	usedBefore, err = QueryTodayEffectiveCostByEndUserForTenant(tenantID, endUserID)
	if err != nil {
		return 0, 0, err
	}
	rawToday, err = QueryRawTodayCostByEndUserForTenant(tenantID, endUserID)
	if err != nil {
		return 0, 0, err
	}
	now := time.Now().UTC()
	_, err = db.Exec(`
		INSERT INTO end_user_daily_spending_resets (tenant_id, end_user_id, day_key, cost_baseline, reset_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (tenant_id, end_user_id) DO UPDATE SET
			day_key = excluded.day_key,
			cost_baseline = excluded.cost_baseline,
			reset_at = excluded.reset_at
	`, tenantID, endUserID, LocalDayKeyAt(now), rawToday, now.Format(time.RFC3339Nano))
	if err != nil {
		return 0, 0, fmt.Errorf("usage: reset today cost by end user: %w", err)
	}
	return usedBefore, rawToday, nil
}
