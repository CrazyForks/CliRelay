package aiaccountstatus

import (
	"context"
	"strings"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

func probeClaude(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	body, err := doAuthGET(ctx, svc, auth, claudeUsageURL, map[string]string{
		"Accept":         "application/json, text/plain, */*",
		"Content-Type":   "application/json",
		"User-Agent":     "claude-code/2.1.7",
		"anthropic-beta": "oauth-2025-04-20",
	}, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Quotas: parseClaudeUsage(body)}, nil
}

func parseClaudeUsage(body []byte) []usage.QuotaWindowDTO {
	keys := []struct {
		path, key, label string
		window           int64
	}{
		{"five_hour", "five_hour", "claude_quota.five_hour", 18000},
		{"seven_day", "seven_day", "claude_quota.seven_day", 604800},
		{"seven_day_oauth_apps", "seven_day_oauth_apps", "claude_quota.seven_day_oauth_apps", 604800},
		{"seven_day_opus", "seven_day_opus", "claude_quota.seven_day_opus", 604800},
		{"seven_day_sonnet", "seven_day_sonnet", "claude_quota.seven_day_sonnet", 604800},
		{"seven_day_cowork", "seven_day_cowork", "claude_quota.seven_day_cowork", 604800},
		{"iguana_necktie", "iguana_necktie", "claude_quota.iguana_necktie", 0},
	}
	out := make([]usage.QuotaWindowDTO, 0, len(keys)+1)
	root := gjson.ParseBytes(body)
	for _, k := range keys {
		win := root.Get(k.path)
		if !win.Exists() {
			continue
		}
		dto := usage.QuotaWindowDTO{QuotaKey: k.key, QuotaLabel: k.label, WindowSeconds: k.window}
		if utilPct := win.Get("utilization"); utilPct.Exists() {
			remaining := 100 - clampPct(utilPct.Float())
			dto.Percent = &remaining
		}
		if reset := firstJSONResult(win, "resets_at", "resetsAt"); reset.Exists() {
			dto.ResetAt = parseFlexibleTime(reset)
		}
		if dto.Percent != nil || dto.ResetAt != nil {
			out = append(out, dto)
		}
	}
	extra := root.Get("extra_usage")
	if extra.Exists() && extra.Get("is_enabled").Bool() {
		if utilization := extra.Get("utilization"); utilization.Exists() {
			remaining := 100 - clampPct(utilization.Float())
			used := strings.TrimSpace(extra.Get("used_credits").String())
			limit := strings.TrimSpace(extra.Get("monthly_limit").String())
			meta := ""
			if used != "" && limit != "" {
				meta = used + " / " + limit + " credits"
			}
			out = append(out, usage.QuotaWindowDTO{
				QuotaKey: "extra_usage", QuotaLabel: "claude_quota.extra_usage_label", Percent: &remaining, Meta: meta,
			})
		}
	}
	return out
}
