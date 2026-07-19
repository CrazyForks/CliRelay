package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// EndUserQuota is the account-level limit/permission snapshot used by auth + quota.
type EndUserQuota struct {
	ID          string
	TenantID    string
	DisplayName string
	// Status is end_users.status (active/disabled/locked). Auth must refuse non-active accounts.
	Status               string
	PermissionProfileID  string
	DailyLimit           int
	TotalQuota           int
	SpendingLimit        float64
	DailySpendingLimit   float64
	ConcurrencyLimit     int
	RPMLimit             int
	TPMLimit             int
	AllowedModels        []string
	AllowedChannels      []string
	AllowedChannelGroups []string
	SystemPrompt         string
}

func decodeQuotaStringList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// GetEndUserQuota loads account-level quota for an end user. nil when missing.
func GetEndUserQuota(endUserID string) *EndUserQuota {
	db := getReadDB()
	if db == nil {
		return nil
	}
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	var q EndUserQuota
	var modelsJSON, channelsJSON, groupsJSON string
	err := db.QueryRow(`
		SELECT id, tenant_id, display_name, COALESCE(status, 'active'),
			COALESCE(permission_profile_id, ''), COALESCE(daily_limit, 0), COALESCE(total_quota, 0),
			COALESCE(spending_limit, 0), COALESCE(daily_spending_limit, 0),
			COALESCE(concurrency_limit, 0), COALESCE(rpm_limit, 0), COALESCE(tpm_limit, 0),
			COALESCE(allowed_models, '[]'), COALESCE(allowed_channels, '[]'), COALESCE(allowed_channel_groups, '[]'),
			COALESCE(system_prompt, '')
		FROM end_users WHERE id = ?
	`, endUserID).Scan(
		&q.ID, &q.TenantID, &q.DisplayName, &q.Status,
		&q.PermissionProfileID, &q.DailyLimit, &q.TotalQuota,
		&q.SpendingLimit, &q.DailySpendingLimit,
		&q.ConcurrencyLimit, &q.RPMLimit, &q.TPMLimit,
		&modelsJSON, &channelsJSON, &groupsJSON, &q.SystemPrompt,
	)
	if err != nil {
		return nil
	}
	q.AllowedModels = decodeQuotaStringList(modelsJSON)
	q.AllowedChannels = decodeQuotaStringList(channelsJSON)
	q.AllowedChannelGroups = decodeQuotaStringList(groupsJSON)
	return &q
}

// EffectiveEndUserQuota merges a permission profile over the end-user row when set.
func EffectiveEndUserQuota(q EndUserQuota) EndUserQuota {
	profileID := strings.TrimSpace(q.PermissionProfileID)
	if profileID == "" {
		return q
	}
	profiles := ListAPIKeyPermissionProfilesForTenant(q.TenantID)
	for _, profile := range profiles {
		if strings.TrimSpace(profile.ID) != profileID {
			continue
		}
		q.PermissionProfileID = profileID
		q.DailyLimit = profile.DailyLimit
		q.TotalQuota = profile.TotalQuota
		q.SpendingLimit = 0
		q.DailySpendingLimit = profile.DailySpendingLimit
		q.ConcurrencyLimit = profile.ConcurrencyLimit
		q.RPMLimit = profile.RPMLimit
		q.TPMLimit = profile.TPMLimit
		q.AllowedModels = append([]string(nil), profile.AllowedModels...)
		q.AllowedChannels = append([]string(nil), profile.AllowedChannels...)
		q.AllowedChannelGroups = append([]string(nil), profile.AllowedChannelGroups...)
		q.SystemPrompt = profile.SystemPrompt
		return q
	}
	return q
}

// ListAPIKeyIDsForEndUser returns stable key ids owned by the end user.
func ListAPIKeyIDsForEndUser(endUserID string) []string {
	return listAPIKeyValuesForEndUser("", endUserID, "id")
}

func ListAPIKeyIDsForEndUserForTenant(tenantID, endUserID string) []string {
	return listAPIKeyValuesForEndUser(tenantID, endUserID, "id")
}

func listAPIKeyValuesForEndUser(tenantID, endUserID, column string) []string {
	db := getReadDB()
	if db == nil {
		return nil
	}
	endUserID = strings.TrimSpace(endUserID)
	if endUserID == "" {
		return nil
	}
	query := `SELECT ` + column + ` FROM api_keys WHERE end_user_id = ? AND ` + column + ` != ''`
	args := []interface{}{endUserID}
	if strings.TrimSpace(tenantID) != "" {
		query += ` AND tenant_id = ?`
		args = append(args, normalizeTenantID(tenantID))
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out
		}
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// ListAPIKeySecretsForEndUser returns raw key secrets owned by the end user (legacy log rows).
func ListAPIKeySecretsForEndUser(endUserID string) []string {
	return listAPIKeyValuesForEndUser("", endUserID, "key")
}

func ListAPIKeySecretsForEndUserForTenant(tenantID, endUserID string) []string {
	return listAPIKeyValuesForEndUser(tenantID, endUserID, "key")
}

// ExpandPublicLookupAPIKeys expands a presented API key into the full account key pool
// when the key is owned by an end user (shared quota / portal multi-key).
// Standalone keys and unknown secrets stay single-key.
func ExpandPublicLookupAPIKeys(apiKey string) []string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return nil
	}
	row := GetAPIKey(trimmed)
	if row == nil {
		return []string{trimmed}
	}
	endUserID := strings.TrimSpace(row.EndUserID)
	if endUserID == "" {
		if k := strings.TrimSpace(row.Key); k != "" {
			return []string{k}
		}
		return []string{trimmed}
	}
	secrets := ListAPIKeySecretsForEndUserForTenant(row.TenantID, endUserID)
	if len(secrets) == 0 {
		if k := strings.TrimSpace(row.Key); k != "" {
			return []string{k}
		}
		return []string{trimmed}
	}
	return secrets
}

// ResolveAPIKeyOwnName returns the key's own name (not end-user account display name).
func ResolveAPIKeyOwnName(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return ""
	}
	row := GetAPIKey(trimmed)
	if row == nil {
		return ""
	}
	return strings.TrimSpace(row.Name)
}

// DisplayNameForEndUser returns display_name for request-log labeling.
func DisplayNameForEndUser(endUserID string) string {
	q := GetEndUserQuota(endUserID)
	if q == nil {
		return ""
	}
	return strings.TrimSpace(q.DisplayName)
}

// ResolveAPIKeyDisplayName prefers end-user display name when the key is owned.
func ResolveAPIKeyDisplayName(row *APIKeyRow, fallback string) string {
	if row != nil {
		if name := DisplayNameForEndUser(row.EndUserID); name != "" {
			return name
		}
		if n := strings.TrimSpace(row.Name); n != "" {
			return n
		}
	}
	return strings.TrimSpace(fallback)
}

func buildEndUserAPIKeySelectorPredicate(tenantID, endUserID string) (string, []interface{}) {
	ids := ListAPIKeyIDsForEndUserForTenant(tenantID, endUserID)
	secrets := ListAPIKeySecretsForEndUserForTenant(tenantID, endUserID)
	if len(ids) == 0 && len(secrets) == 0 {
		// No keys yet: match nothing.
		return "1 = 0", nil
	}
	parts := make([]string, 0, 2)
	args := make([]interface{}, 0, len(ids)+len(secrets))
	if len(ids) > 0 {
		ph := make([]string, len(ids))
		for i, id := range ids {
			ph[i] = "?"
			args = append(args, id)
		}
		parts = append(parts, `api_key_id IN (`+strings.Join(ph, ",")+`)`)
	}
	if len(secrets) > 0 {
		ph := make([]string, len(secrets))
		for i, key := range secrets {
			ph[i] = "?"
			args = append(args, key)
		}
		// Include legacy rows that only stored the raw secret.
		parts = append(parts, `(trim(coalesce(api_key_id, '')) = '' AND api_key IN (`+strings.Join(ph, ",")+`))`)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func buildEndUserAPIKeySelectorClause(endUserID string) (string, []interface{}) {
	quota := GetEndUserQuota(endUserID)
	tenantID := ""
	if quota != nil {
		tenantID = quota.TenantID
	}
	predicate, args := buildEndUserAPIKeySelectorPredicate(tenantID, endUserID)
	return " WHERE " + predicate, args
}

// CountTodayByEndUser counts requests across all keys of the end user today.
func CountTodayByEndUser(endUserID string) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	clause, args := buildEndUserAPIKeySelectorClause(endUserID)
	if clause == "" {
		return 0, nil
	}
	queryArgs := append(args, CutoffStartUTC(1).Format(time.RFC3339))
	var count int64
	err := db.QueryRow(
		"SELECT COUNT(*) FROM request_logs"+clause+" AND timestamp >= ?",
		queryArgs...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: count today by end user: %w", err)
	}
	return count, nil
}

// CountTotalByEndUser counts lifetime requests across all keys of the end user.
func CountTotalByEndUser(endUserID string) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	clause, args := buildEndUserAPIKeySelectorClause(endUserID)
	if clause == "" {
		return 0, nil
	}
	var count int64
	err := db.QueryRow("SELECT COUNT(*) FROM request_logs"+clause, args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: count total by end user: %w", err)
	}
	return count, nil
}

// QueryTotalCostByEndUser sums lifetime cost across all keys of the end user.
func QueryTotalCostByEndUser(endUserID string) (float64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	clause, args := buildEndUserAPIKeySelectorClause(endUserID)
	if clause == "" {
		return 0, nil
	}
	var total float64
	err := db.QueryRow(
		"SELECT COALESCE(SUM(cost), 0) FROM request_logs"+clause,
		args...,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("usage: query total cost by end user: %w", err)
	}
	return total, nil
}

// QueryTodayCostByEndUser returns account-level project-day spend after the latest same-day reset baseline.
func QueryTodayCostByEndUser(endUserID string) (float64, error) {
	quota := GetEndUserQuota(endUserID)
	if quota == nil {
		return 0, nil
	}
	return QueryTodayEffectiveCostByEndUserForTenant(quota.TenantID, endUserID)
}

// EnsureEndUserQuotaColumns adds account quota columns on SQLite bootstraps / tests.
func EnsureEndUserQuotaColumns(db *sql.DB) error {
	if db == nil {
		return nil
	}
	for _, col := range []struct {
		name string
		def  string
	}{
		{"permission_profile_id", "TEXT NOT NULL DEFAULT ''"},
		{"daily_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"total_quota", "INTEGER NOT NULL DEFAULT 0"},
		{"spending_limit", "REAL NOT NULL DEFAULT 0"},
		{"daily_spending_limit", "REAL NOT NULL DEFAULT 0"},
		{"concurrency_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"rpm_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"tpm_limit", "INTEGER NOT NULL DEFAULT 0"},
		{"allowed_models", "TEXT NOT NULL DEFAULT '[]'"},
		{"allowed_channels", "TEXT NOT NULL DEFAULT '[]'"},
		{"allowed_channel_groups", "TEXT NOT NULL DEFAULT '[]'"},
		{"system_prompt", "TEXT NOT NULL DEFAULT ''"},
	} {
		if _, err := db.Exec("ALTER TABLE end_users ADD COLUMN " + col.name + " " + col.def); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				// table may not exist yet in pure-key tests
				msg := strings.ToLower(err.Error())
				if strings.Contains(msg, "no such table") {
					return nil
				}
				return err
			}
		}
	}
	return nil
}
