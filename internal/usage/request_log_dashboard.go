package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// DashboardKPI holds the aggregated KPI data needed by the dashboard page.
type DashboardKPI struct {
	TotalRequests   int64   `json:"total_requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailedRequests  int64   `json:"failed_requests"`
	SuccessRate     float64 `json:"success_rate"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	TotalCost       float64 `json:"total_cost"`
	CacheRate       float64 `json:"cache_rate"`
}

type DashboardTrendPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type DashboardThroughputPoint struct {
	Label string  `json:"label"`
	RPM   float64 `json:"rpm"`
	TPM   float64 `json:"tpm"`
}

type DashboardTrends struct {
	RequestVolume    []DashboardTrendPoint      `json:"request_volume"`
	SuccessRate      []DashboardTrendPoint      `json:"success_rate"`
	TotalTokens      []DashboardTrendPoint      `json:"total_tokens"`
	FailedRequests   []DashboardTrendPoint      `json:"failed_requests"`
	ThroughputSeries []DashboardThroughputPoint `json:"throughput_series"`
}

type dashboardBucket struct {
	label      string
	key        string
	minutes    float64
	requests   int64
	success    int64
	failed     int64
	totalToken int64
}

const dashboardThroughputBucketCount = 7

// QueryDashboardKPI returns aggregated KPI data from SQLite for the dashboard.
// This replaces the old in-memory snapshot-based counting which lost data on restart.
func QueryDashboardKPI(days int) (DashboardKPI, error) {
	return QueryDashboardKPIForTenant(systemTenantID, days)
}

func QueryDashboardKPIForTenant(tenantID string, days int) (DashboardKPI, error) {
	db := getReadDB()
	if db == nil {
		return DashboardKPI{}, nil
	}
	if days < 1 {
		days = 7
	}

	cutoff := CutoffStartUTC(days).Format(time.RFC3339)

	var kpi DashboardKPI
	var effectiveInputTokens int64
	kpiSQL := "SELECT " +
		"COUNT(*), " +
		"COALESCE(SUM(CASE WHEN failed=0 THEN 1 ELSE 0 END), 0), " +
		"COALESCE(SUM(CASE WHEN failed=1 THEN 1 ELSE 0 END), 0), " +
		"COALESCE(SUM(input_tokens), 0), " +
		"COALESCE(SUM(output_tokens), 0), " +
		"COALESCE(SUM(reasoning_tokens), 0), " +
		"COALESCE(SUM(cached_tokens), 0), " +
		"COALESCE(SUM(total_tokens), 0), " +
		"COALESCE(SUM(cost), 0), " +
		"COALESCE(SUM(" + cacheRateEffectiveInputSQL + "), 0) " +
		"FROM request_logs WHERE tenant_id = ? AND timestamp >= ?"
	err := db.QueryRow(kpiSQL, normalizeTenantID(tenantID), cutoff).Scan(
		&kpi.TotalRequests,
		&kpi.SuccessRequests,
		&kpi.FailedRequests,
		&kpi.InputTokens,
		&kpi.OutputTokens,
		&kpi.ReasoningTokens,
		&kpi.CachedTokens,
		&kpi.TotalTokens,
		&kpi.TotalCost,
		&effectiveInputTokens,
	)
	if err != nil {
		return DashboardKPI{}, fmt.Errorf("usage: dashboard KPI query: %w", err)
	}

	if kpi.TotalRequests > 0 {
		kpi.SuccessRate = float64(kpi.SuccessRequests) / float64(kpi.TotalRequests) * 100
	}
	kpi.CacheRate = cacheRateFromTokenTotals(effectiveInputTokens, kpi.CachedTokens)

	return kpi, nil
}

// QueryDashboardTrends returns fixed-width trend buckets used by the dashboard.
// KPI trends follow the selected day range, while throughput always shows the
// most recent 7 points: completed minutes use calendar-minute totals, and the
// latest point is a rolling last-60-seconds window so RPM/TPM stay continuous
// across minute boundaries.
func QueryDashboardTrends(days int) (DashboardTrends, error) {
	return QueryDashboardTrendsForTenant(systemTenantID, days)
}

func QueryDashboardTrendsForTenant(tenantID string, days int) (DashboardTrends, error) {
	db := getReadDB()
	if db == nil {
		return emptyDashboardTrends(days), nil
	}
	if days < 1 {
		days = 7
	}

	loc := getUsageLocation()
	buckets := buildDashboardBuckets(days, loc)
	byKey := make(map[string]*dashboardBucket, len(buckets))
	for i := range buckets {
		byKey[buckets[i].key] = &buckets[i]
	}

	// Aggregate in SQL instead of streaming every matching request_logs row into Go.
	// Dashboard polls this path every few seconds; a full-row scan over multi-day
	// windows was the 2026-07-16 load-incident root cause behind nginx 503 guards.
	// date()/strftime(..., 'localtime') is rewritten by the postgres compat driver
	// to a UTC day/hour key; production keeps process TZ aligned with the usage zone.
	cutoff := CutoffStartUTC(days).Format(time.RFC3339)
	tenantID = normalizeTenantID(tenantID)
	var (
		rows *sql.Rows
		err  error
	)
	if days == 1 {
		// Hourly buckets: key shape is "2006-01-02 15" (no minutes).
		rows, err = db.Query(`
			SELECT strftime('%Y-%m-%d %H:00', timestamp, 'localtime') as h,
			       failed, COUNT(*), COALESCE(SUM(total_tokens), 0)
			FROM request_logs
			WHERE tenant_id = ? AND timestamp >= ?
			GROUP BY h, failed
		`, tenantID, cutoff)
	} else {
		rows, err = db.Query(`
			SELECT date(timestamp, 'localtime') as d,
			       failed, COUNT(*), COALESCE(SUM(total_tokens), 0)
			FROM request_logs
			WHERE tenant_id = ? AND timestamp >= ?
			GROUP BY d, failed
		`, tenantID, cutoff)
	}
	if err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: query dashboard trends: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var bucketKey string
		var failedInt int
		var requests int64
		var totalTokens int64
		if err := rows.Scan(&bucketKey, &failedInt, &requests, &totalTokens); err != nil {
			return DashboardTrends{}, fmt.Errorf("usage: scan dashboard trend row: %w", err)
		}
		key := normalizeDashboardSQLBucketKey(bucketKey, days)
		bucket := byKey[key]
		if bucket == nil {
			continue
		}
		bucket.requests += requests
		bucket.totalToken += totalTokens
		if failedInt != 0 {
			bucket.failed += requests
		} else {
			bucket.success += requests
		}
	}
	if err := rows.Err(); err != nil {
		return DashboardTrends{}, fmt.Errorf("usage: iterate dashboard trends: %w", err)
	}

	throughputSeries, err := queryDashboardThroughputSeriesAt(tenantID, time.Now(), loc, false)
	if err != nil {
		return DashboardTrends{}, err
	}

	trends := dashboardTrendsFromBuckets(buckets)
	trends.ThroughputSeries = throughputSeries
	return trends, nil
}

// normalizeDashboardSQLBucketKey maps SQL group keys onto dashboardBucketKey shapes.
// Hourly SQL uses "YYYY-MM-DD HH:00"; Go keys drop the trailing ":00".
func normalizeDashboardSQLBucketKey(raw string, days int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if days == 1 {
		if strings.HasSuffix(raw, ":00") {
			return strings.TrimSuffix(raw, ":00")
		}
		// Accept already-normalized "YYYY-MM-DD HH".
		if len(raw) == len("2006-01-02 15") {
			return raw
		}
	}
	return raw
}

// QueryDashboardThroughputAcrossTenants returns the recent 7 RPM/TPM points
// aggregated across every tenant (same continuous latest-window semantics as
// tenant trends). Used by platform super-admins.
func QueryDashboardThroughputAcrossTenants() ([]DashboardThroughputPoint, error) {
	return queryDashboardThroughputSeriesAt("", time.Now(), getUsageLocation(), true)
}

func emptyDashboardTrends(days int) DashboardTrends {
	if days < 1 {
		days = 7
	}
	loc := getUsageLocation()
	trends := dashboardTrendsFromBuckets(buildDashboardBuckets(days, loc))
	trends.ThroughputSeries = throughputSeriesFromBuckets(buildRecentThroughputBucketsAt(time.Now(), loc))
	return trends
}

func buildDashboardBuckets(days int, loc *time.Location) []dashboardBucket {
	if loc == nil {
		loc = time.Local
	}
	start := CutoffStartUTC(days).In(loc)
	if days == 1 {
		buckets := make([]dashboardBucket, 0, 24)
		for i := 0; i < 24; i++ {
			at := start.Add(time.Duration(i) * time.Hour)
			buckets = append(buckets, dashboardBucket{
				label:   at.Format("15:04"),
				key:     dashboardBucketKey(at, days),
				minutes: 60,
			})
		}
		return buckets
	}

	buckets := make([]dashboardBucket, 0, days)
	for i := 0; i < days; i++ {
		at := start.AddDate(0, 0, i)
		buckets = append(buckets, dashboardBucket{
			label:   at.Format("2006-01-02"),
			key:     dashboardBucketKey(at, days),
			minutes: 24 * 60,
		})
	}
	return buckets
}

func dashboardBucketKey(t time.Time, days int) string {
	if days == 1 {
		return t.Format("2006-01-02 15")
	}
	return t.Format("2006-01-02")
}

func buildRecentThroughputBucketsAt(now time.Time, loc *time.Location) []dashboardBucket {
	if loc == nil {
		loc = time.Local
	}
	currentMinute := now.In(loc).Truncate(time.Minute)
	start := currentMinute.Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)
	buckets := make([]dashboardBucket, 0, dashboardThroughputBucketCount)
	for i := 0; i < dashboardThroughputBucketCount; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		buckets = append(buckets, dashboardBucket{
			label:   at.Format("15:04"),
			key:     at.Format("2006-01-02 15:04"),
			minutes: 1,
		})
	}
	return buckets
}

func queryDashboardThroughputSeriesAt(tenantID string, now time.Time, loc *time.Location, allTenants bool) ([]DashboardThroughputPoint, error) {
	db := getReadDB()
	if db == nil {
		return throughputSeriesFromBuckets(buildRecentThroughputBucketsAt(now, loc)), nil
	}
	if loc == nil {
		loc = time.Local
	}

	now = now.In(loc)
	buckets := buildRecentThroughputBucketsAt(now, loc)
	byKey := make(map[string]*dashboardBucket, len(buckets))
	for i := range buckets {
		byKey[buckets[i].key] = &buckets[i]
	}

	// Completed minute points stay calendar-aligned; the latest point is filled
	// from a rolling 60s window so the dashboard value does not drop to zero at
	// each minute boundary.
	windowStart := now.Add(-time.Minute)
	start := now.Truncate(time.Minute).Add(-time.Duration(dashboardThroughputBucketCount-1) * time.Minute)
	if windowStart.Before(start) {
		start = windowStart
	}
	startUTC := start.UTC().Format(time.RFC3339)

	var (
		rows *sql.Rows
		err  error
	)
	if allTenants {
		rows, err = db.Query(`
			SELECT timestamp, total_tokens
			FROM request_logs
			WHERE timestamp >= ?
		`, startUTC)
	} else {
		rows, err = db.Query(`
			SELECT timestamp, total_tokens
			FROM request_logs
			WHERE tenant_id = ? AND timestamp >= ?
		`, normalizeTenantID(tenantID), startUTC)
	}
	if err != nil {
		return nil, fmt.Errorf("usage: query dashboard throughput trends: %w", err)
	}
	defer rows.Close()

	var (
		windowRequests int64
		windowTokens   int64
	)
	for rows.Next() {
		var ts storedTime
		var totalTokens int64
		if err := rows.Scan(&ts, &totalTokens); err != nil {
			return nil, fmt.Errorf("usage: scan dashboard throughput row: %w", err)
		}
		if !ts.Valid {
			continue
		}
		at := ts.Time.In(loc)
		// Historical points: full calendar minute.
		key := at.Truncate(time.Minute).Format("2006-01-02 15:04")
		if bucket := byKey[key]; bucket != nil {
			bucket.requests++
			bucket.totalToken += totalTokens
		}
		// Latest point: requests that finished in the last 60 seconds.
		if !at.Before(windowStart) && !at.After(now) {
			windowRequests++
			windowTokens += totalTokens
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate dashboard throughput rows: %w", err)
	}

	if len(buckets) > 0 {
		// Replace the in-progress calendar minute with the rolling window so the
		// rightmost chart point and headline RPM/TPM stay continuous.
		buckets[len(buckets)-1].requests = windowRequests
		buckets[len(buckets)-1].totalToken = windowTokens
	}

	return throughputSeriesFromBuckets(buckets), nil
}

func dashboardTrendsFromBuckets(buckets []dashboardBucket) DashboardTrends {
	trends := DashboardTrends{
		RequestVolume:    make([]DashboardTrendPoint, 0, len(buckets)),
		SuccessRate:      make([]DashboardTrendPoint, 0, len(buckets)),
		TotalTokens:      make([]DashboardTrendPoint, 0, len(buckets)),
		FailedRequests:   make([]DashboardTrendPoint, 0, len(buckets)),
		ThroughputSeries: make([]DashboardThroughputPoint, 0),
	}

	for _, bucket := range buckets {
		successRate := 0.0
		if bucket.requests > 0 {
			successRate = float64(bucket.success) / float64(bucket.requests) * 100
		}

		trends.RequestVolume = append(trends.RequestVolume, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.requests)})
		trends.SuccessRate = append(trends.SuccessRate, DashboardTrendPoint{Label: bucket.label, Value: successRate})
		trends.TotalTokens = append(trends.TotalTokens, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.totalToken)})
		trends.FailedRequests = append(trends.FailedRequests, DashboardTrendPoint{Label: bucket.label, Value: float64(bucket.failed)})
	}

	return trends
}

func throughputSeriesFromBuckets(buckets []dashboardBucket) []DashboardThroughputPoint {
	points := make([]DashboardThroughputPoint, 0, len(buckets))
	for _, bucket := range buckets {
		rpm := 0.0
		tpm := 0.0
		if bucket.minutes > 0 {
			rpm = float64(bucket.requests) / bucket.minutes
			tpm = float64(bucket.totalToken) / bucket.minutes
		}
		points = append(points, DashboardThroughputPoint{
			Label: bucket.label,
			RPM:   rpm,
			TPM:   tpm,
		})
	}
	return points
}
