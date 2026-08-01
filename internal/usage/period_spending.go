package usage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

var ErrQuotaUsageUnavailable = errors.New("quota usage unavailable")

type periodSubject string

const (
	periodSubjectAPIKey  periodSubject = "api_key_id"
	periodSubjectEndUser periodSubject = "end_user_id"
)

type PeriodWindowKeys struct {
	FiveHourFrom string
	FiveHourTo   string
	Day          string
	WeekFrom     string
	MonthFrom    string
	DayTo        string
}

func PeriodWindowKeysAt(now time.Time, loc *time.Location) PeriodWindowKeys {
	if loc == nil {
		loc = time.Local
	}
	nowMinute := now.UTC().Truncate(time.Minute)
	localNow := now.In(loc)
	localMidnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	weekdayOffset := (int(localMidnight.Weekday()) + 6) % 7
	weekStart := localMidnight.AddDate(0, 0, -weekdayOffset)
	monthStart := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, loc)
	return PeriodWindowKeys{
		FiveHourFrom: nowMinute.Add(-5 * time.Hour).Format("2006-01-02T15:04"),
		FiveHourTo:   nowMinute.Add(time.Minute).Format("2006-01-02T15:04"),
		Day:          localDayKeyAtLocation(now, loc),
		WeekFrom:     localDayKeyAtLocation(weekStart, loc),
		MonthFrom:    localDayKeyAtLocation(monthStart, loc),
		DayTo:        localDayKeyAtLocation(localMidnight.AddDate(0, 0, 1), loc),
	}
}

func QueryPeriodSpendingByAPIKeyIDForTenant(tenantID, apiKeyID string) (quota.PeriodSpendingUsage, error) {
	values, err := QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantID, []string{apiKeyID}, time.Now())
	if err != nil {
		return quota.PeriodSpendingUsage{}, err
	}
	return values[strings.TrimSpace(apiKeyID)], nil
}

func QueryPeriodSpendingByEndUserForTenant(tenantID, endUserID string) (quota.PeriodSpendingUsage, error) {
	values, err := QueryPeriodSpendingByEndUsersForTenantAt(tenantID, []string{endUserID}, time.Now())
	if err != nil {
		return quota.PeriodSpendingUsage{}, err
	}
	return values[strings.TrimSpace(endUserID)], nil
}

func QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantID string, apiKeyIDs []string, now time.Time) (map[string]quota.PeriodSpendingUsage, error) {
	return queryPeriodSpendingForSubjects(tenantID, periodSubjectAPIKey, apiKeyIDs, now)
}

func QueryPeriodSpendingByEndUsersForTenantAt(tenantID string, endUserIDs []string, now time.Time) (map[string]quota.PeriodSpendingUsage, error) {
	return queryPeriodSpendingForSubjects(tenantID, periodSubjectEndUser, endUserIDs, now)
}

func QueryPeriodSpendingByAPIKeyIDsForTenant(tenantID string, apiKeyIDs []string) (map[string]quota.PeriodSpendingUsage, error) {
	return QueryPeriodSpendingByAPIKeyIDsForTenantAt(tenantID, apiKeyIDs, time.Now())
}

func QueryPeriodSpendingByEndUsersForTenant(tenantID string, endUserIDs []string) (map[string]quota.PeriodSpendingUsage, error) {
	return QueryPeriodSpendingByEndUsersForTenantAt(tenantID, endUserIDs, time.Now())
}

func queryRawPeriodSpendingForSubjects(tenantID string, subject periodSubject, ids []string, now time.Time) (map[string]quota.PeriodSpendingUsage, error) {
	ids = dedupeExactStrings(ids)
	out := make(map[string]quota.PeriodSpendingUsage, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = quota.PeriodSpendingUsage{}
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	const subjectChunkSize = 300
	if len(out) > subjectChunkSize {
		cleanIDs := make([]string, 0, len(out))
		for id := range out {
			cleanIDs = append(cleanIDs, id)
		}
		combined := make(map[string]quota.PeriodSpendingUsage, len(cleanIDs))
		for start := 0; start < len(cleanIDs); start += subjectChunkSize {
			end := start + subjectChunkSize
			if end > len(cleanIDs) {
				end = len(cleanIDs)
			}
			part, err := queryRawPeriodSpendingForSubjects(tenantID, subject, cleanIDs[start:end], now)
			if err != nil {
				return nil, err
			}
			for id, used := range part {
				combined[id] = used
			}
		}
		return combined, nil
	}
	ids = ids[:0]
	for id := range out {
		ids = append(ids, id)
	}
	windows := PeriodWindowKeysAt(now, getUsageLocation())
	queries := []struct {
		kind string
		from string
		to   string
		set  func(*quota.PeriodSpendingUsage, float64)
	}{
		{rollupBucketQuotaMinuteUTC, windows.FiveHourFrom, windows.FiveHourTo, func(v *quota.PeriodSpendingUsage, n float64) { v.FiveHour = n }},
		{rollupBucketDay, windows.Day, windows.DayTo, func(v *quota.PeriodSpendingUsage, n float64) { v.Day = n }},
		{rollupBucketDay, windows.WeekFrom, windows.DayTo, func(v *quota.PeriodSpendingUsage, n float64) { v.Week = n }},
		{rollupBucketDay, windows.MonthFrom, windows.DayTo, func(v *quota.PeriodSpendingUsage, n float64) { v.Month = n }},
		{rollupBucketLifetime, "", "", func(v *quota.PeriodSpendingUsage, n float64) { v.Lifetime = n }},
	}
	for _, query := range queries {
		values, err := queryGroupedPeriodCost(tenantID, subject, ids, query.kind, query.from, query.to)
		if err != nil {
			return nil, err
		}
		for id, value := range values {
			current := out[id]
			query.set(&current, value)
			out[id] = current
		}
	}

	return out, nil
}

func queryPeriodSpendingForSubjects(tenantID string, subject periodSubject, ids []string, now time.Time) (map[string]quota.PeriodSpendingUsage, error) {
	out, err := queryRawPeriodSpendingForSubjects(tenantID, subject, ids, now)
	if err != nil || len(out) == 0 {
		return out, err
	}
	cleanIDs := make([]string, 0, len(out))
	for id := range out {
		cleanIDs = append(cleanIDs, id)
	}
	windows := PeriodWindowKeysAt(now, getUsageLocation())
	baselines, err := listEffectivePeriodBaselines(tenantID, subject, cleanIDs, windows)
	if err != nil {
		return nil, fmt.Errorf("%w: period baselines: %v", ErrQuotaUsageUnavailable, err)
	}
	for id, periodBaselines := range baselines {
		current := out[id]
		for period, baseline := range periodBaselines {
			applyPeriodBaseline(&current, period, baseline)
		}
		out[id] = current
	}
	return out, nil
}

func listEffectivePeriodBaselines(tenantID string, subject periodSubject, ids []string, windows PeriodWindowKeys) (map[string]map[quota.Period]float64, error) {
	const baselineChunkSize = 300
	if len(ids) > baselineChunkSize {
		combined := make(map[string]map[quota.Period]float64)
		for start := 0; start < len(ids); start += baselineChunkSize {
			end := start + baselineChunkSize
			if end > len(ids) {
				end = len(ids)
			}
			part, err := listEffectivePeriodBaselines(tenantID, subject, ids[start:end], windows)
			if err != nil {
				return nil, err
			}
			for id, values := range part {
				combined[id] = values
			}
		}
		return combined, nil
	}
	out, err := listPeriodSpendingResetBaselines(tenantID, subject, ids, windows)
	if err != nil {
		return nil, err
	}
	var legacy map[string]float64
	if subject == periodSubjectAPIKey {
		legacy, err = listDailySpendingResetBaselinesAt(tenantID, ids, windows.Day)
	} else {
		legacy, err = listEndUserDailySpendingResetBaselines(tenantID, ids, windows.Day)
	}
	if err != nil {
		return nil, err
	}
	for id, baseline := range legacy {
		if out[id] == nil {
			out[id] = make(map[quota.Period]float64)
		}
		if _, exists := out[id][quota.PeriodDay]; !exists {
			out[id][quota.PeriodDay] = baseline
		}
	}
	return out, nil
}

func queryGroupedPeriodCost(tenantID string, subject periodSubject, ids []string, kind, from, to string) (map[string]float64, error) {
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	column := string(subject)
	if subject != periodSubjectAPIKey && subject != periodSubjectEndUser {
		return nil, fmt.Errorf("%w: invalid subject", ErrQuotaUsageUnavailable)
	}
	var b strings.Builder
	b.WriteString(`SELECT ` + column + `, COALESCE(SUM(cost_total), 0) FROM usage_rollup_buckets WHERE tenant_id = ? AND bucket_kind = ? AND ` + column + ` IN (` + placeholders(len(ids)) + `)`)
	args := make([]any, 0, len(ids)+4)
	args = append(args, normalizeTenantID(tenantID), kind)
	for _, id := range ids {
		args = append(args, id)
	}
	if from != "" {
		b.WriteString(` AND bucket_start >= ?`)
		args = append(args, from)
	}
	if to != "" {
		b.WriteString(` AND bucket_start < ?`)
		args = append(args, to)
	}
	b.WriteString(` GROUP BY ` + column)
	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("%w: query %s %s: %v", ErrQuotaUsageUnavailable, subject, kind, err)
	}
	defer rows.Close()
	out := make(map[string]float64, len(ids))
	for rows.Next() {
		var id string
		var value float64
		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("%w: scan %s %s: %v", ErrQuotaUsageUnavailable, subject, kind, err)
		}
		out[id] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows %s %s: %v", ErrQuotaUsageUnavailable, subject, kind, err)
	}
	return out, nil
}

func listEndUserDailySpendingResetBaselines(tenantID string, ids []string, dayKey string) (map[string]float64, error) {
	db := getReadDB()
	if db == nil {
		return nil, ErrQuotaUsageUnavailable
	}
	query := `SELECT end_user_id, cost_baseline FROM end_user_daily_spending_resets WHERE tenant_id = ? AND day_key = ? AND end_user_id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids)+2)
	args = append(args, normalizeTenantID(tenantID), dayKey)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]float64{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64, len(ids))
	for rows.Next() {
		var id string
		var baseline float64
		if err := rows.Scan(&id, &baseline); err != nil {
			return nil, err
		}
		out[id] = baseline
	}
	return out, rows.Err()
}
