package aiaccountstatus

import (
	"context"
	"fmt"
	"math"
	"strings"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	xaiBillingWeeklyURL  = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	xaiBillingMonthlyURL = "https://cli-chat-proxy.grok.com/v1/billing"
)

func probeXAI(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	headers := map[string]string{
		"x-xai-token-auth":      "xai-grok-cli",
		"x-grok-client-version": "0.2.91",
		"accept":                "*/*",
		"user-agent":            "grok-pager/0.2.91 grok-shell/0.2.91 (macos; aarch64)",
	}
	if userID := resolveXAIUserID(auth); userID != "" {
		headers["x-userid"] = userID
	}
	// Parallel weekly/monthly under the same probe slot (does not take extra global semaphore).
	weeklyBody, weeklyErr, monthlyBody, monthlyErr := fetchXAIBillingParallel(ctx,
		func(ctx context.Context) ([]byte, error) {
			return doAuthGET(ctx, svc, auth, xaiBillingWeeklyURL, headers, nil)
		},
		func(ctx context.Context) ([]byte, error) {
			return doAuthGET(ctx, svc, auth, xaiBillingMonthlyURL, headers, nil)
		},
	)
	if weeklyErr != nil && monthlyErr != nil {
		return ProbeResult{}, weeklyErr
	}

	quotas := make([]usage.QuotaWindowDTO, 0, 8)
	if weeklyErr == nil {
		quotas = append(quotas, parseXAIWeeklyBilling(weeklyBody)...)
	} else if monthlyErr == nil {
		quotas = append(quotas, parseXAIWeeklyBilling(monthlyBody)...)
	}
	if monthlyErr == nil {
		quotas = append(quotas, parseXAIMonthlyBilling(monthlyBody)...)
	} else if weeklyErr == nil {
		quotas = append(quotas, parseXAIMonthlyBilling(weeklyBody)...)
	}
	if len(quotas) == 0 {
		return ProbeResult{}, fmt.Errorf("empty_data")
	}
	planBody := monthlyBody
	if monthlyErr != nil {
		planBody = weeklyBody
	}
	return ProbeResult{Quotas: quotas, PlanType: resolveXAIPlan(planBody)}, nil
}

// fetchXAIBillingParallel runs weekly and monthly fetches concurrently and
// preserves partial-success merge semantics (one side may fail).
func fetchXAIBillingParallel(ctx context.Context, fetchWeekly, fetchMonthly func(context.Context) ([]byte, error)) (weeklyBody []byte, weeklyErr error, monthlyBody []byte, monthlyErr error) {
	type getResult struct {
		body []byte
		err  error
	}
	weeklyCh := make(chan getResult, 1)
	monthlyCh := make(chan getResult, 1)
	go func() {
		if fetchWeekly == nil {
			weeklyCh <- getResult{err: fmt.Errorf("weekly fetch unavailable")}
			return
		}
		body, err := fetchWeekly(ctx)
		weeklyCh <- getResult{body: body, err: err}
	}()
	go func() {
		if fetchMonthly == nil {
			monthlyCh <- getResult{err: fmt.Errorf("monthly fetch unavailable")}
			return
		}
		body, err := fetchMonthly(ctx)
		monthlyCh <- getResult{body: body, err: err}
	}()
	weekly := <-weeklyCh
	monthly := <-monthlyCh
	return weekly.body, weekly.err, monthly.body, monthly.err
}

func resolveXAIUserID(auth *coreauth.Auth) string {
	return firstNonEmpty(
		authString(auth, "sub", "subject", "user_id", "userId", "x_userid"),
		metadataNestedString(auth, "oauth", "sub", "subject", "user_id", "userId"),
		metadataNestedString(auth, "user", "sub", "subject", "id", "user_id", "userId"),
	)
}

func parseXAIBilling(body []byte, key, _ string, _ int64) []usage.QuotaWindowDTO {
	if key == "weekly_limit" {
		return parseXAIWeeklyBilling(body)
	}
	return parseXAIMonthlyBilling(body)
}

func parseXAIWeeklyBilling(body []byte) []usage.QuotaWindowDTO {
	cfg := gjson.GetBytes(body, "config")
	if !cfg.Exists() {
		return nil
	}
	current := firstJSONResult(cfg, "currentPeriod", "current_period")
	periodType := strings.ToLower(strings.TrimSpace(current.Get("type").String()))
	used := firstJSONResult(cfg, "creditUsagePercent", "credit_usage_percent")
	products := firstJSONResult(cfg, "productUsage", "product_usage")
	if !used.Exists() && !strings.Contains(periodType, "weekly") && !products.IsArray() {
		return nil
	}

	remaining := 100.0
	if used.Exists() {
		remaining = math.Round(100 - clampPct(used.Float()))
	}
	reset := firstJSONResult(current, "end")
	if !reset.Exists() {
		reset = firstJSONResult(cfg, "billingPeriodEnd", "billing_period_end")
	}
	// Weekly cards already show relative reset from ResetAt; keep Meta empty so the
	// UI is not flooded with raw ISO period strings like "2026-07-16T06:45:51+00:00 - …".
	out := []usage.QuotaWindowDTO{{
		QuotaKey: "weekly_limit", QuotaLabel: "xai_quota.weekly_limit", Percent: &remaining,
		Value: formatPercent(remaining), ResetAt: parseFlexibleTime(reset), WindowSeconds: 604800,
	}}
	if products.IsArray() {
		index := 0
		products.ForEach(func(_, product gjson.Result) bool {
			index++
			name := strings.TrimSpace(product.Get("product").String())
			if name == "" {
				name = fmt.Sprintf("Product %d", index)
			}
			productRemaining := 100.0
			if productUsed := firstJSONResult(product, "usagePercent", "usage_percent"); productUsed.Exists() {
				productRemaining = math.Round(100 - clampPct(productUsed.Float()))
			}
			out = append(out, usage.QuotaWindowDTO{
				QuotaKey: "product:" + name, QuotaLabel: "xai_quota.product_usage_named::" + name,
				Percent: &productRemaining, Value: formatPercent(productRemaining),
			})
			return true
		})
	}
	return out
}

func parseXAIMonthlyBilling(body []byte) []usage.QuotaWindowDTO {
	cfg := gjson.GetBytes(body, "config")
	if !cfg.Exists() {
		return nil
	}
	monthlyLimit, hasMonthlyLimit := xaiCentValue(cfg, "monthlyLimit", "monthly_limit")
	used, hasUsed := xaiCentValue(cfg, "used")
	onDemandCap, hasOnDemandCap := xaiCentValue(cfg, "onDemandCap", "on_demand_cap")
	onDemandUsed, hasOnDemandUsed := xaiCentValue(cfg, "onDemandUsed", "on_demand_used")
	billingEnd := firstJSONResult(cfg, "billingPeriodEnd", "billing_period_end")
	hasMonthlyData := hasMonthlyLimit || hasUsed || hasOnDemandCap || billingEnd.Exists()
	if !hasMonthlyData {
		return nil
	}

	if !hasOnDemandUsed && hasUsed && hasMonthlyLimit {
		onDemandUsed = math.Max(0, used-monthlyLimit)
		hasOnDemandUsed = true
	}
	out := make([]usage.QuotaWindowDTO, 0, 2)
	payGoRemaining := 100.0
	payGoMeta := ""
	if hasOnDemandCap && onDemandCap > 0 {
		if hasOnDemandUsed {
			payGoRemaining = math.Round(100 - clampPct((onDemandUsed/onDemandCap)*100))
		}
		remainingCents := 0.0
		if hasOnDemandUsed {
			remainingCents = math.Max(0, onDemandCap-onDemandUsed)
		}
		payGoMeta = formatUSDCents(remainingCents) + " / " + formatUSDCents(onDemandCap)
	}
	out = append(out, usage.QuotaWindowDTO{
		QuotaKey: "pay_as_you_go", QuotaLabel: "xai_quota.pay_as_you_go_label",
		Percent: &payGoRemaining, Value: formatPercent(payGoRemaining), Meta: payGoMeta,
	})

	if hasMonthlyLimit || hasUsed || billingEnd.Exists() {
		includedUsed := used
		if hasUsed && hasMonthlyLimit && monthlyLimit > 0 {
			includedUsed = math.Min(used, monthlyLimit)
		}
		monthlyRemaining := 100.0
		if hasMonthlyLimit && monthlyLimit > 0 && hasUsed {
			monthlyRemaining = math.Round(100 - clampPct((includedUsed/monthlyLimit)*100))
		}
		remainingCents := 0.0
		if hasMonthlyLimit && hasUsed {
			remainingCents = math.Max(0, monthlyLimit-includedUsed)
		}
		meta := ""
		if hasMonthlyLimit {
			meta = formatUSDCents(remainingCents) + " / " + formatUSDCents(monthlyLimit)
		}
		out = append(out, usage.QuotaWindowDTO{
			QuotaKey: "monthly_credits", QuotaLabel: "xai_quota.monthly_credits",
			Percent: &monthlyRemaining, Value: formatPercent(monthlyRemaining), ResetAt: parseFlexibleTime(billingEnd), Meta: meta,
		})
	}
	return out
}

func xaiCentValue(cfg gjson.Result, paths ...string) (float64, bool) {
	value := firstJSONResult(cfg, paths...)
	if !value.Exists() {
		return 0, false
	}
	if value.IsObject() {
		value = value.Get("val")
	}
	if !value.Exists() {
		return 0, false
	}
	return value.Float(), true
}

func resolveXAIPlan(body []byte) string {
	cfg := gjson.GetBytes(body, "config")
	limit, ok := xaiCentValue(cfg, "monthlyLimit", "monthly_limit")
	if !ok {
		return ""
	}
	switch int64(math.Round(limit)) {
	case 15000:
		return "supergrok"
	case 150000:
		return "supergrok-heavy"
	default:
		return ""
	}
}

func formatPercent(percent float64) string {
	return fmt.Sprintf("%.0f%%", math.Round(clampPct(percent)))
}

func formatUSDCents(cents float64) string {
	return fmt.Sprintf("$%.2f", cents/100)
}
