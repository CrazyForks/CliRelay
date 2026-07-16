package aiaccountstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const defaultAntigravityProject = "bamboo-precept-lgxtn"

var antigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels",
	"https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
}

func probeAntigravity(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	projectID := firstNonEmpty(
		metadataString(auth, "project_id", "projectId"),
		metadataNestedString(auth, "installed", "project_id", "projectId"),
		metadataNestedString(auth, "web", "project_id", "projectId"),
		defaultAntigravityProject,
	)
	payloadJSON, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return ProbeResult{}, fmt.Errorf("encode antigravity project: %w", err)
	}
	payload := string(payloadJSON)
	var lastErr error
	for _, url := range antigravityQuotaURLs {
		body, err := doAuthPOST(ctx, svc, auth, url, map[string]string{
			"Content-Type": "application/json",
			"User-Agent":   "antigravity/1.11.5 windows/amd64",
		}, payload)
		if err != nil {
			lastErr = err
			continue
		}
		quotas := parseAntigravityModels(body)
		if len(quotas) == 0 {
			lastErr = fmt.Errorf("no_model_quota")
			continue
		}
		return ProbeResult{Quotas: quotas}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request_failed")
	}
	return ProbeResult{}, lastErr
}

type antigravityQuotaGroup struct {
	key, label string
	models     map[string]struct{}
}

var antigravityQuotaGroups = []antigravityQuotaGroup{
	{key: "provider:gemini3-pro", label: "antigravity_quota.gemini3_pro", models: stringSet("gemini-3-pro-low", "gemini-3-pro-high", "gemini-3-pro-preview", "gemini-3.1-pro-low", "gemini-3.1-pro-high", "gemini-3.1-pro-preview")},
	{key: "provider:gemini3-flash", label: "antigravity_quota.gemini3_flash", models: stringSet("gemini-3-flash", "gemini-3-flash-agent")},
	{key: "provider:gemini-image", label: "antigravity_quota.gemini_image", models: stringSet("gemini-2.5-flash-image", "gemini-3.1-flash-image", "gemini-3-pro-image", "gemini-3-pro-image-preview")},
	{key: "provider:claude", label: "antigravity_quota.claude", models: stringSet("claude-fable-5", "claude-sonnet-4-5", "claude-sonnet-4-5-thinking", "claude-opus-4-5-thinking", "claude-sonnet-4-6", "claude-opus-4-6", "claude-opus-4-6-thinking", "claude-opus-4-7", "claude-opus-4-8")},
}

var antigravitySkippedModels = stringSet(
	"chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview",
	"gemini-2.5-flash-thinking", "gemini-2.5-pro",
)

func parseAntigravityModels(body []byte) []usage.QuotaWindowDTO {
	root := gjson.ParseBytes(body)
	models := root.Get("models")
	if !models.IsObject() && root.IsObject() {
		models = root
	}
	if !models.IsObject() {
		return nil
	}
	type groupValue struct {
		percent *float64
		resetAt *time.Time
		count   int
	}
	grouped := make(map[string]*groupValue, len(antigravityQuotaGroups))
	models.ForEach(func(modelID, model gjson.Result) bool {
		id := strings.ToLower(strings.TrimSpace(modelID.String()))
		id = strings.TrimPrefix(id, "models/")
		if id == "" {
			return true
		}
		if _, skip := antigravitySkippedModels[id]; skip {
			return true
		}
		var group *antigravityQuotaGroup
		for i := range antigravityQuotaGroups {
			if _, ok := antigravityQuotaGroups[i].models[id]; ok {
				group = &antigravityQuotaGroups[i]
				break
			}
		}
		if group == nil {
			return true
		}
		info := firstJSONResult(model, "quotaInfo", "quota_info")
		fraction := firstJSONResult(info, "remainingFraction", "remaining_fraction", "remaining")
		resetAt := parseFlexibleTime(firstJSONResult(info, "resetTime", "reset_time"))
		if !fraction.Exists() && resetAt == nil {
			return true
		}
		current := grouped[group.key]
		if current == nil {
			current = &groupValue{}
			grouped[group.key] = current
		}
		current.count++
		if fraction.Exists() {
			fractionValue := quotaFraction(fraction)
			if fractionValue == nil {
				return true
			}
			remaining := math.Round(clampPct(*fractionValue * 100))
			if current.percent == nil || remaining < *current.percent {
				current.percent = &remaining
			}
		}
		if resetAt != nil && (current.resetAt == nil || resetAt.Before(*current.resetAt)) {
			current.resetAt = resetAt
		}
		return true
	})

	out := make([]usage.QuotaWindowDTO, 0, len(grouped))
	for _, group := range antigravityQuotaGroups {
		value := grouped[group.key]
		if value == nil || value.count == 0 {
			continue
		}
		out = append(out, usage.QuotaWindowDTO{
			QuotaKey: group.key, QuotaLabel: group.label, Percent: value.percent, ResetAt: value.resetAt,
		})
	}
	return out
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
