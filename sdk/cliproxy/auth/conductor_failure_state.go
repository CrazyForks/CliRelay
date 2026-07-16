package auth

import "time"

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	// 402 weekly balance exhausted (xAI Grok Build) is a quota event, not a
	// short-lived payment_required probe. Keep generic 402/403 on 30m.
	if statusCode == 402 && isQuotaExhaustionError(resultErr, retryAfter) {
		applyAuthQuotaFailureState(auth, resultErr, retryAfter, now)
		return
	}
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 402, 403:
		auth.StatusMessage = "payment_required"
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 404:
		auth.StatusMessage = "not_found"
		auth.NextRetryAfter = now.Add(12 * time.Hour)
	case 429:
		applyAuthQuotaFailureState(auth, resultErr, retryAfter, now)
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if quotaCooldownDisabledForAuth(auth) {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

func applyAuthQuotaFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	if resultErr != nil && resultErr.Message != "" {
		auth.StatusMessage = resultErr.Message
	} else {
		auth.StatusMessage = "quota exhausted"
	}
	auth.Quota.Exceeded = true
	auth.Quota.Reason = "quota"
	if resultErr != nil {
		auth.Quota.Window = resultErr.QuotaWindow
		auth.Quota.WindowMinutes = resultErr.QuotaWindowMinutes
	}
	var next time.Time
	if retryAfter != nil {
		next = now.Add(*retryAfter)
	} else {
		// WindowMinutes is window length metadata (e.g. week=10080), not remaining cooldown.
		cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, quotaCooldownDisabledForAuth(auth))
		if cooldown > 0 {
			next = now.Add(cooldown)
		}
		auth.Quota.BackoffLevel = nextLevel
	}
	auth.Quota.NextRecoverAt = next
	auth.NextRetryAfter = next
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}
