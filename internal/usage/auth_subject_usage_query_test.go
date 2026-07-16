package usage

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Regression: AI Accounts card trend used to SELECT every matching request_logs
// row for the 7-day daily series and aggregate in Go, which pegged CPU on large
// tenants. Daily totals must stay correct after SQL-side GROUP BY.
func TestQueryDailyUsageByAuthSubjectAggregatesInSQL(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	loc := getUsageLocation()
	nowLocal := time.Now().In(loc)
	// Place three requests: two on "today" local day, one on yesterday local day.
	todayMorning := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 9, 15, 0, 0, loc)
	todayAfternoon := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 14, 45, 0, 0, loc)
	yesterday := todayMorning.AddDate(0, 0, -1)

	if err := UpsertModelPricing("gpt-5.4", 1, 1, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	matcher := AuthSubjectMatcher{AuthIndexes: []string{"auth-sql-agg"}}
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, yesterday.UTC(), 10, 1, TokenStats{
		InputTokens: 1000, OutputTokens: 0, TotalTokens: 1000,
	}, "", "")
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, todayMorning.UTC(), 10, 1, TokenStats{
		InputTokens: 1000, OutputTokens: 0, TotalTokens: 1000,
	}, "", "")
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, todayAfternoon.UTC(), 10, 1, TokenStats{
		InputTokens: 2000, OutputTokens: 0, TotalTokens: 2000,
	}, "", "")
	// Noise row for a different auth_index must not appear.
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-other", false, todayMorning.UTC(), 10, 1, TokenStats{
		InputTokens: 9999, OutputTokens: 0, TotalTokens: 9999,
	}, "", "")

	daily, err := QueryDailyUsageByAuthSubject(matcher, 3)
	if err != nil {
		t.Fatalf("QueryDailyUsageByAuthSubject: %v", err)
	}
	byDate := map[string]DailyUsagePoint{}
	for _, point := range daily {
		byDate[point.Date] = point
	}
	todayKey := todayMorning.Format("2006-01-02")
	yesterdayKey := yesterday.Format("2006-01-02")
	if got := byDate[todayKey]; got.Requests != 2 {
		t.Fatalf("today requests = %d cost=%v, want 2 (points=%+v)", got.Requests, got.Cost, daily)
	}
	if got := byDate[yesterdayKey]; got.Requests != 1 {
		t.Fatalf("yesterday requests = %d, want 1 (points=%+v)", got.Requests, daily)
	}
	// Pricing is $1 / 1M input tokens → 1000+2000 tokens today = 0.003
	if math.Abs(byDate[todayKey].Cost-0.003) > 1e-9 {
		t.Fatalf("today cost = %v, want ~0.003", byDate[todayKey].Cost)
	}

	// Hourly path remains a narrow window; just ensure it still sees recent rows.
	recent := time.Now().Add(-30 * time.Minute)
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-sql-agg", false, recent.UTC(), 10, 1, TokenStats{
		InputTokens: 500, OutputTokens: 0, TotalTokens: 500,
	}, "", "")

	hourly, err := QueryHourlyUsageByAuthSubject(matcher, 5)
	if err != nil {
		t.Fatalf("QueryHourlyUsageByAuthSubject: %v", err)
	}
	if len(hourly) != 5 {
		t.Fatalf("hourly len = %d, want 5", len(hourly))
	}
	var hourlyTotal int64
	for _, point := range hourly {
		hourlyTotal += point.Requests
	}
	if hourlyTotal < 1 {
		t.Fatalf("hourly total requests = %d, want >= 1 (buckets=%+v)", hourlyTotal, hourly)
	}
}
