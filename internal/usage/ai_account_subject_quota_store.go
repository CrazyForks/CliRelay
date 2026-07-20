package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type AIAccountSubjectQuotaCycle struct {
	AuthSubjectID  string
	Provider       string
	QuotaKey       string
	CycleStartAt   time.Time
	ResetAt        time.Time
	WindowSeconds  int64
	LastVerifiedAt time.Time
}

func RecordAIAccountSubjectQuotaPoints(authSubjectID, provider string, points []QuotaSnapshotPoint) error {
	db := getDB()
	authSubjectID = strings.TrimSpace(authSubjectID)
	provider = strings.TrimSpace(provider)
	if db == nil || authSubjectID == "" || len(points) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: shared quota begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	for _, point := range points {
		key := strings.TrimSpace(point.QuotaKey)
		if key == "" {
			continue
		}
		recordedAt := point.RecordedAt
		if recordedAt.IsZero() {
			recordedAt = now
		}
		recordedAt = recordedAt.UTC()
		pointProvider := strings.TrimSpace(point.Provider)
		if pointProvider == "" {
			pointProvider = provider
		}
		label := strings.TrimSpace(point.QuotaLabel)
		if label == "" {
			label = key
		}
		if point.Percent != nil {
			v := *point.Percent
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			point.Percent = &v
		}
		if point.ResetAt != nil && !point.ResetAt.IsZero() && point.WindowSeconds > 0 {
			cycle := AIAccountSubjectQuotaCycle{
				AuthSubjectID: authSubjectID, Provider: pointProvider, QuotaKey: key,
				CycleStartAt: point.ResetAt.UTC().Add(-time.Duration(point.WindowSeconds) * time.Second),
				ResetAt:      point.ResetAt.UTC(), WindowSeconds: point.WindowSeconds, LastVerifiedAt: recordedAt,
			}
			if err := upsertAIAccountSubjectQuotaCycleTx(tx, cycle); err != nil {
				return err
			}
			setAIAccountSubjectActiveCycle(cycle)
		}
		var latestAt sql.NullString
		var latestPercent sql.NullFloat64
		var latestReset sql.NullString
		var latestWindow int64
		err := tx.QueryRow(`
			SELECT recorded_at, percent, reset_at, window_seconds
			FROM ai_account_subject_quota_points
			WHERE auth_subject_id = ? AND quota_key = ?
			ORDER BY recorded_at DESC LIMIT 1
		`, authSubjectID, key).Scan(&latestAt, &latestPercent, &latestReset, &latestWindow)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			latest, _ := parseStoredTimeString(latestAt.String)
			samePercent := (!latestPercent.Valid && point.Percent == nil) || (latestPercent.Valid && point.Percent != nil && latestPercent.Float64 == *point.Percent)
			sameReset := nullableStoredTimeEqual(latestReset, point.ResetAt)
			if recordedAt.Sub(latest) < quotaSnapshotHeartbeatInterval && samePercent && sameReset && latestWindow == point.WindowSeconds {
				continue
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO ai_account_subject_quota_points
				(auth_subject_id, provider, quota_key, quota_label, percent, reset_at, window_seconds, recorded_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, authSubjectID, pointProvider, key, label, point.Percent, nullableTimeValue(point.ResetAt), point.WindowSeconds, recordedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("usage: insert shared quota point: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func nullableStoredTimeEqual(stored sql.NullString, value *time.Time) bool {
	if !stored.Valid || strings.TrimSpace(stored.String) == "" {
		return value == nil || value.IsZero()
	}
	if value == nil || value.IsZero() {
		return false
	}
	parsed, ok := parseStoredTimeString(stored.String)
	return ok && parsed.Equal(value.UTC())
}

func upsertAIAccountSubjectQuotaCycleTx(tx *sql.Tx, cycle AIAccountSubjectQuotaCycle) error {
	_, err := tx.Exec(`
		INSERT INTO ai_account_subject_quota_cycles
			(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id, quota_key) DO UPDATE SET
			provider = excluded.provider,
			cycle_start_at = excluded.cycle_start_at,
			reset_at = excluded.reset_at,
			window_seconds = excluded.window_seconds,
			last_verified_at = excluded.last_verified_at
	`, cycle.AuthSubjectID, cycle.Provider, cycle.QuotaKey,
		cycle.CycleStartAt.UTC().Format(time.RFC3339Nano), cycle.ResetAt.UTC().Format(time.RFC3339Nano),
		cycle.WindowSeconds, cycle.LastVerifiedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("usage: upsert shared quota cycle: %w", err)
	}
	return nil
}

func QueryLatestAIAccountSubjectWeeklyCyclesBatch(subjectIDs []string, preferredKeys []string) (map[string]time.Time, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	out := make(map[string]time.Time)
	if db == nil || len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids)+len(preferredKeys))
	for _, id := range ids {
		args = append(args, id)
	}
	query := `
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)
		  AND window_seconds >= 604800`
	keys := dedupeExactStrings(preferredKeys)
	if len(keys) > 0 {
		query += ` AND quota_key IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`
		for _, key := range keys {
			args = append(args, key)
		}
	}
	query += ` ORDER BY last_verified_at DESC, reset_at DESC`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared quota cycles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			return nil, err
		}
		if start.Valid {
			cycle.CycleStartAt = start.Time
		}
		if reset.Valid {
			cycle.ResetAt = reset.Time
		}
		if verified.Valid {
			cycle.LastVerifiedAt = verified.Time
		}
		setAIAccountSubjectActiveCycle(cycle)
		if _, exists := out[cycle.AuthSubjectID]; !exists {
			out[cycle.AuthSubjectID] = cycle.CycleStartAt
		}
	}
	return out, rows.Err()
}

func loadAIAccountSubjectCycleCache(db *sql.DB) error {
	resetAIAccountSubjectCycleCache()
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM ai_account_subject_quota_cycles WHERE window_seconds > 0
		ORDER BY last_verified_at DESC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var cycle AIAccountSubjectQuotaCycle
		var start, reset, verified storedTime
		if err := rows.Scan(&cycle.AuthSubjectID, &cycle.Provider, &cycle.QuotaKey, &start, &reset, &cycle.WindowSeconds, &verified); err != nil {
			return err
		}
		if _, ok := seen[cycle.AuthSubjectID]; ok {
			continue
		}
		seen[cycle.AuthSubjectID] = struct{}{}
		if start.Valid {
			cycle.CycleStartAt = start.Time
		}
		if reset.Valid {
			cycle.ResetAt = reset.Time
		}
		if verified.Valid {
			cycle.LastVerifiedAt = verified.Time
		}
		setAIAccountSubjectActiveCycle(cycle)
	}
	return rows.Err()
}

func cleanupExpiredAIAccountSubjectQuotaPoints(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -8).UTC().Format(time.RFC3339Nano)
	res, err := db.Exec(`DELETE FROM ai_account_subject_quota_points WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
