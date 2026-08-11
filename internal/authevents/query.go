package authevents

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ListFilter narrows an attempt listing.
type ListFilter struct {
	IPPrefix string
	Username string
	Outcome  string
	Surface  string
	Since    time.Time
	Until    time.Time
	Page     int
	Size     int
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
	// maxSummarySources caps the aggregation result. An unbounded GROUP BY over
	// an attack window could return hundreds of thousands of rows straight into
	// the panel.
	maxSummarySources = 100
)

func (f ListFilter) normalized() ListFilter {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Size < 1 {
		f.Size = defaultPageSize
	}
	if f.Size > maxPageSize {
		f.Size = maxPageSize
	}
	return f
}

func (f ListFilter) conditions() ([]string, []any) {
	where := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if value := strings.TrimSpace(f.IPPrefix); value != "" {
		where = append(where, "(ip_prefix = ? OR ip = ?)")
		args = append(args, value, value)
	}
	if value := strings.TrimSpace(f.Username); value != "" {
		where = append(where, "username ILIKE ?")
		args = append(args, "%"+value+"%")
	}
	if value := strings.TrimSpace(f.Outcome); value != "" {
		where = append(where, "outcome = ?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(f.Surface); value != "" {
		where = append(where, "surface = ?")
		args = append(args, value)
	}
	if !f.Since.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, f.Since.UTC())
	}
	if !f.Until.IsZero() {
		where = append(where, "occurred_at <= ?")
		args = append(args, f.Until.UTC())
	}
	return where, args
}

// List returns a page of attempts with the total row count.
func (r *Recorder) List(ctx context.Context, filter ListFilter) ([]Event, int, error) {
	if !r.Available() {
		return nil, 0, nil
	}
	filter = filter.normalized()
	where, args := filter.conditions()
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_attempt_events`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `SELECT id,occurred_at,ip,ip_prefix,trusted,scope,surface,username,outcome,reason,user_agent,request_path,request_id,tenant_id
		FROM auth_attempt_events` + clause + ` ORDER BY occurred_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Size, (filter.Page-1)*filter.Size)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	events := make([]Event, 0, filter.Size)
	for rows.Next() {
		var (
			event    Event
			tenantID sql.NullString
		)
		if err = rows.Scan(&event.ID, &event.OccurredAt, &event.IP, &event.IPPrefix, &event.Trusted,
			&event.Scope, &event.Surface, &event.Username, &event.Outcome, &event.Reason,
			&event.UserAgent, &event.RequestPath, &event.RequestID, &tenantID); err != nil {
			return nil, 0, err
		}
		if tenantID.Valid {
			event.TenantID = tenantID.String
		}
		events = append(events, event)
	}
	return events, total, rows.Err()
}

// Summarize aggregates attempts by source over a window.
//
// Ordering puts the noisiest failing sources first, because the question this
// answers is "who should I ban", not "who visited". Successes are counted but
// never rank a source up.
func (r *Recorder) Summarize(ctx context.Context, since time.Time, limit int) ([]SourceSummary, error) {
	if !r.Available() {
		return nil, nil
	}
	if limit < 1 || limit > maxSummarySources {
		limit = maxSummarySources
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT ip_prefix,
		       MAX(ip)                                                        AS sample_ip,
		       BOOL_OR(trusted)                                               AS trusted,
		       COUNT(*)                                                       AS attempts,
		       COUNT(*) FILTER (WHERE outcome = 'failure')                    AS failures,
		       COUNT(*) FILTER (WHERE outcome = 'throttled')                  AS throttled,
		       COUNT(*) FILTER (WHERE outcome IN ('blocked','auto_banned'))   AS blocked,
		       COUNT(*) FILTER (WHERE outcome = 'success')                    AS successes,
		       COUNT(DISTINCT NULLIF(username, ''))                           AS distinct_usernames,
		       MIN(occurred_at)                                               AS first_seen,
		       MAX(occurred_at)                                               AS last_seen,
		       STRING_AGG(DISTINCT surface, ',')                              AS surfaces
		  FROM auth_attempt_events
		 WHERE occurred_at >= ? AND ip_prefix <> ''
		 GROUP BY ip_prefix
		 ORDER BY failures DESC, attempts DESC
		 LIMIT ?`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	summaries := make([]SourceSummary, 0, limit)
	for rows.Next() {
		var (
			summary  SourceSummary
			sampleIP sql.NullString
			surfaces sql.NullString
		)
		if err = rows.Scan(&summary.IPPrefix, &sampleIP, &summary.Trusted, &summary.Attempts,
			&summary.Failures, &summary.Throttled, &summary.Blocked, &summary.Successes,
			&summary.DistinctUser, &summary.FirstSeen, &summary.LastSeen, &surfaces); err != nil {
			return nil, err
		}
		if sampleIP.Valid {
			summary.SampleIP = sampleIP.String
		}
		if surfaces.Valid {
			summary.Surfaces = surfaces.String
		}
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

// Purge deletes attempts older than cutoff and reports how many rows went.
func (r *Recorder) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	if !r.Available() {
		return 0, nil
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM auth_attempt_events WHERE occurred_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
