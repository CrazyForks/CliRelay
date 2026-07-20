package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

const aiAccountSubjectDayRetention = 400 * 24 * time.Hour

var sharedCycleCache = struct {
	sync.RWMutex
	bySubject map[string]AIAccountSubjectQuotaCycle
}{bySubject: make(map[string]AIAccountSubjectQuotaCycle)}

func resetAIAccountSubjectCycleCache() {
	sharedCycleCache.Lock()
	sharedCycleCache.bySubject = make(map[string]AIAccountSubjectQuotaCycle)
	sharedCycleCache.Unlock()
}

func setAIAccountSubjectActiveCycle(cycle AIAccountSubjectQuotaCycle) {
	if strings.TrimSpace(cycle.AuthSubjectID) == "" || cycle.CycleStartAt.IsZero() || cycle.ResetAt.IsZero() || cycle.WindowSeconds <= 0 {
		return
	}
	sharedCycleCache.Lock()
	sharedCycleCache.bySubject[cycle.AuthSubjectID] = cycle
	sharedCycleCache.Unlock()
}

func aiAccountSubjectCycleAt(subjectID string, at time.Time) (AIAccountSubjectQuotaCycle, bool) {
	sharedCycleCache.RLock()
	cycle, ok := sharedCycleCache.bySubject[strings.TrimSpace(subjectID)]
	sharedCycleCache.RUnlock()
	if !ok || cycle.WindowSeconds <= 0 || cycle.CycleStartAt.IsZero() || cycle.ResetAt.IsZero() {
		return AIAccountSubjectQuotaCycle{}, false
	}
	at = at.UTC()
	window := time.Duration(cycle.WindowSeconds) * time.Second
	for !at.Before(cycle.ResetAt) {
		cycle.CycleStartAt = cycle.ResetAt
		cycle.ResetAt = cycle.ResetAt.Add(window)
	}
	return cycle, !at.Before(cycle.CycleStartAt)
}

// projectAIAccountSubjectUsageTx is the request-hot B-layer projection. It only
// uses the server-computed subject already captured by usageReporter; it never
// reads or upserts the low-frequency tenant binding table.
func projectAIAccountSubjectUsageTx(tx *sql.Tx, authSubjectID string, failed bool, cost float64, at time.Time) error {
	if tx == nil {
		return nil
	}
	authSubjectID = strings.TrimSpace(authSubjectID)
	if authSubjectID == "" {
		return nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	loc := usageLoc
	if loc == nil {
		loc = time.Local
	}
	successInc, failureInc := int64(1), int64(0)
	if failed {
		successInc, failureInc = 0, 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	first := at.UTC().Format(time.RFC3339Nano)
	buckets := []struct{ kind, start string }{
		{kind: "day", start: at.In(loc).Format("2006-01-02")},
		{kind: "lifetime", start: rollupLifetimeStart},
	}
	if cycle, ok := aiAccountSubjectCycleAt(authSubjectID, at); ok {
		buckets = append(buckets, struct{ kind, start string }{kind: "cycle", start: cycle.CycleStartAt.UTC().Format(time.RFC3339Nano)})
	}
	const upsert = `
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count,
			success_count, failure_count, cost_total, first_event_at, updated_at
		) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
			request_count = ai_account_subject_usage_buckets.request_count + 1,
			success_count = ai_account_subject_usage_buckets.success_count + excluded.success_count,
			failure_count = ai_account_subject_usage_buckets.failure_count + excluded.failure_count,
			cost_total = ai_account_subject_usage_buckets.cost_total + excluded.cost_total,
			updated_at = excluded.updated_at
	`
	// The fixed day -> lifetime -> cycle order is shared by every writer.
	for _, bucket := range buckets {
		if _, err := tx.Exec(upsert, authSubjectID, bucket.kind, bucket.start, successInc, failureInc, cost, first, now); err != nil {
			return fmt.Errorf("usage: project shared subject %s: %w", bucket.kind, err)
		}
	}
	return nil
}

func QueryAIAccountSubjectUsageSummaries(subjectIDs []string, cycleStartBySubject map[string]time.Time) (map[string]AuthSubjectUsageSummary, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	out := make(map[string]AuthSubjectUsageSummary, len(ids))
	for _, id := range ids {
		out[id] = AuthSubjectUsageSummary{AuthSubjectID: id}
	}
	if db == nil || len(ids) == 0 {
		return out, nil
	}

	loc := getUsageLocation()
	now := time.Now().In(loc)
	start7 := now.AddDate(0, 0, -6).Format("2006-01-02")
	start30 := now.AddDate(0, 0, -29).Format("2006-01-02")
	args := make([]any, 0, len(ids)+1)
	args = append(args, start30)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT auth_subject_id,
			SUM(CASE WHEN bucket_start >= ? THEN request_count ELSE 0 END),
			SUM(CASE WHEN bucket_start >= ? THEN cost_total ELSE 0 END),
			SUM(request_count), SUM(success_count), SUM(failure_count), MAX(updated_at)
		FROM ai_account_subject_usage_buckets
		WHERE bucket_kind = 'day' AND bucket_start >= ?
		  AND auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
		GROUP BY auth_subject_id
	`, append([]any{start7, start7}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("usage: query shared subject day summaries: %w", err)
	}
	for rows.Next() {
		var id string
		var r7 int64
		var c7 float64
		var r30, s30, f30 int64
		var updated sql.NullString
		if err := rows.Scan(&id, &r7, &c7, &r30, &s30, &f30, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		s := out[id]
		s.RequestTotal7d, s.CostTotal7d = r7, c7
		s.RequestTotal30d, s.SuccessTotal30d, s.FailureTotal30d = r30, s30, f30
		if t, ok := parseStoredTimeString(updated.String); ok {
			s.UpdatedAt = t
		}
		out[id] = s
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	lifeArgs := make([]any, 0, len(ids))
	for _, id := range ids {
		lifeArgs = append(lifeArgs, id)
	}
	lifeRows, err := db.Query(`
		SELECT u.auth_subject_id, u.request_count, u.success_count, u.failure_count, u.cost_total,
			u.first_event_at, u.updated_at, s.usage_projected_since, s.usage_history_complete
		FROM ai_account_subject_usage_buckets u
		LEFT JOIN ai_account_subjects s ON s.auth_subject_id = u.auth_subject_id
		WHERE u.bucket_kind = 'lifetime' AND u.bucket_start = '1970-01-01'
		  AND u.auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
	`, lifeArgs...)
	if err != nil {
		return nil, err
	}
	for lifeRows.Next() {
		var id string
		var requestTotal, successTotal, failureTotal int64
		var costTotal float64
		var first, updated, projected sql.NullString
		var complete bool
		if err := lifeRows.Scan(&id, &requestTotal, &successTotal, &failureTotal, &costTotal, &first, &updated, &projected, &complete); err != nil {
			lifeRows.Close()
			return nil, err
		}
		s := out[id]
		s.AuthSubjectID = id
		s.RequestTotal = requestTotal
		s.SuccessTotal = successTotal
		s.FailureTotal = failureTotal
		s.CostTotal = costTotal
		denom := s.SuccessTotal + s.FailureTotal
		if denom > 0 {
			rate := float64(s.SuccessTotal) / float64(denom)
			s.SuccessRate = &rate
		}
		s.ProjectedSince = parseNullableTime(projected)
		if s.ProjectedSince == nil {
			s.ProjectedSince = parseNullableTime(first)
		}
		s.HistoryComplete = complete
		if t, ok := parseStoredTimeString(updated.String); ok && t.After(s.UpdatedAt) {
			s.UpdatedAt = t
		}
		out[id] = s
	}
	if err := lifeRows.Close(); err != nil {
		return nil, err
	}

	cycleIDs := make([]string, 0, len(cycleStartBySubject))
	cycleStarts := make([]string, 0, len(cycleStartBySubject))
	startSeen := make(map[string]struct{}, len(cycleStartBySubject))
	for id, start := range cycleStartBySubject {
		if _, ok := out[id]; !ok || start.IsZero() {
			continue
		}
		startKey := start.UTC().Format(time.RFC3339Nano)
		s := out[id]
		s.CycleKnown = true
		s.CycleStart = start.UTC().Format(time.RFC3339)
		out[id] = s
		cycleIDs = append(cycleIDs, id)
		if _, ok := startSeen[startKey]; !ok {
			startSeen[startKey] = struct{}{}
			cycleStarts = append(cycleStarts, startKey)
		}
	}
	if len(cycleIDs) == 0 {
		return out, nil
	}

	cycleArgs := make([]any, 0, len(cycleIDs)+len(cycleStarts))
	for _, id := range cycleIDs {
		cycleArgs = append(cycleArgs, id)
	}
	for _, start := range cycleStarts {
		cycleArgs = append(cycleArgs, start)
	}
	cycleRows, err := db.Query(`
		SELECT auth_subject_id, bucket_start, request_count, cost_total, updated_at
		FROM ai_account_subject_usage_buckets
		WHERE bucket_kind = 'cycle'
		  AND auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(cycleIDs)), ",")+`)
		  AND bucket_start IN (`+strings.TrimSuffix(strings.Repeat("?,", len(cycleStarts)), ",")+`)
	`, cycleArgs...)
	if err != nil {
		return nil, err
	}
	for cycleRows.Next() {
		var id, bucketStart string
		var req int64
		var cost float64
		var updated sql.NullString
		if err := cycleRows.Scan(&id, &bucketStart, &req, &cost, &updated); err != nil {
			cycleRows.Close()
			return nil, err
		}
		expected, ok := cycleStartBySubject[id]
		if !ok || bucketStart != expected.UTC().Format(time.RFC3339Nano) {
			continue
		}
		s := out[id]
		s.CycleRequestTotal, s.CycleCostTotal = req, cost
		if t, ok := parseStoredTimeString(updated.String); ok && t.After(s.UpdatedAt) {
			s.UpdatedAt = t
		}
		out[id] = s
	}
	if err := cycleRows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func cleanupExpiredAIAccountSubjectUsageBuckets(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-aiAccountSubjectDayRetention).In(getUsageLocation()).Format("2006-01-02")
	res, err := db.Exec(`DELETE FROM ai_account_subject_usage_buckets WHERE bucket_kind = 'day' AND bucket_start < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
