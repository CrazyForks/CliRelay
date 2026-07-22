package xai

import (
	"math"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	// BillingWeeklyPath is the Grok Build weekly included-usage endpoint.
	BillingWeeklyPath = "/billing?format=credits"
	// CLIWeeklyBillingURL is the production Grok Build weekly included-usage URL.
	CLIWeeklyBillingURL = CLIChatProxyBaseURL + BillingWeeklyPath
)

// WeeklyBilling describes the quota fields shared by runtime recovery probes and
// the management account-status view.
type WeeklyBilling struct {
	RemainingPercent float64
	ResetAt          time.Time
	Products         []WeeklyProductBilling
}

// WeeklyProductBilling describes one product-specific weekly usage entry.
type WeeklyProductBilling struct {
	Name             string
	RemainingPercent float64
}

// ParseWeeklyBilling parses the Grok Build billing response's weekly usage data.
func ParseWeeklyBilling(body []byte) (WeeklyBilling, bool) {
	cfg := gjson.GetBytes(body, "config")
	if !cfg.Exists() {
		return WeeklyBilling{}, false
	}
	current := firstBillingResult(cfg, "currentPeriod", "current_period")
	periodType := strings.ToLower(strings.TrimSpace(current.Get("type").String()))
	used := firstBillingResult(cfg, "creditUsagePercent", "credit_usage_percent")
	products := firstBillingResult(cfg, "productUsage", "product_usage")
	if !used.Exists() && !strings.Contains(periodType, "weekly") && !products.IsArray() {
		return WeeklyBilling{}, false
	}

	weekly := WeeklyBilling{RemainingPercent: 100}
	if used.Exists() {
		weekly.RemainingPercent = math.Round(100 - clampBillingPercent(used.Float()))
	}
	reset := firstBillingResult(current, "end")
	if !reset.Exists() {
		reset = firstBillingResult(cfg, "billingPeriodEnd", "billing_period_end")
	}
	weekly.ResetAt = parseBillingTime(reset)

	if products.IsArray() {
		weekly.Products = make([]WeeklyProductBilling, 0)
		products.ForEach(func(_, product gjson.Result) bool {
			item := WeeklyProductBilling{
				Name:             strings.TrimSpace(product.Get("product").String()),
				RemainingPercent: 100,
			}
			if productUsed := firstBillingResult(product, "usagePercent", "usage_percent"); productUsed.Exists() {
				item.RemainingPercent = math.Round(100 - clampBillingPercent(productUsed.Float()))
			}
			weekly.Products = append(weekly.Products, item)
			return true
		})
	}
	return weekly, true
}

func firstBillingResult(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if value := root.Get(path); value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func parseBillingTime(value gjson.Result) time.Time {
	if !value.Exists() {
		return time.Time{}
	}
	raw := strings.TrimSpace(value.String())
	if raw != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, raw); err == nil {
				return parsed.UTC()
			}
		}
	}
	seconds := value.Float()
	if seconds <= 0 {
		return time.Time{}
	}
	if seconds > 1e12 {
		seconds /= 1000
	}
	return time.Unix(int64(seconds), int64((seconds-float64(int64(seconds)))*float64(time.Second))).UTC()
}

func clampBillingPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
