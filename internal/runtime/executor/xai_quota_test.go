package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestNewXAIStatusErr_BalanceExhaustedMapsToWeeklyQuota(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":"Grok Build usage balance exhausted"}`)
	err := newXAIStatusErr(http.StatusPaymentRequired, body)

	if err.StatusCode() != http.StatusPaymentRequired {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusPaymentRequired)
	}
	window, minutes := err.QuotaWindow()
	if window != "week" || minutes != 10080 {
		t.Fatalf("QuotaWindow() = %q/%d, want week/10080", window, minutes)
	}
	// Unknown remaining time: do not invent now+7d from window length.
	if got := err.RetryAfter(); got != nil {
		t.Fatalf("RetryAfter() = %v, want nil when upstream omits reset", *got)
	}
}

func TestNewXAIStatusErr_PrefersRetryAfterHeader(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":"Grok Build usage balance exhausted"}`)
	headers := http.Header{"Retry-After": []string{"3600"}}
	err := newXAIStatusErr(http.StatusPaymentRequired, body, headers)

	retryAfter := err.RetryAfter()
	if retryAfter == nil {
		t.Fatal("RetryAfter() = nil")
	}
	if *retryAfter != time.Hour {
		t.Fatalf("RetryAfter() = %v, want 1h from header", *retryAfter)
	}
	window, minutes := err.QuotaWindow()
	if window != "week" || minutes != 10080 {
		t.Fatalf("QuotaWindow() = %q/%d, want week/10080", window, minutes)
	}
}

func TestNewXAIStatusErr_UsesResetsInSeconds(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"message":"Grok Build usage balance exhausted","resets_in_seconds":7200}}`)
	err := newXAIStatusErr(http.StatusPaymentRequired, body)
	retryAfter := err.RetryAfter()
	if retryAfter == nil {
		t.Fatal("RetryAfter() = nil")
	}
	if *retryAfter != 2*time.Hour {
		t.Fatalf("RetryAfter() = %v, want 2h", *retryAfter)
	}
}

func TestNewXAIStatusErr_Generic402HasNoWeeklyQuota(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":"payment required"}`)
	err := newXAIStatusErr(http.StatusPaymentRequired, body)
	window, minutes := err.QuotaWindow()
	if window != "" || minutes != 0 {
		t.Fatalf("QuotaWindow() = %q/%d, want empty", window, minutes)
	}
	if got := err.RetryAfter(); got != nil {
		t.Fatalf("RetryAfter() = %v, want nil for generic 402", *got)
	}
}

func TestIsXAIUsageBalanceExhausted_NestedMessage(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"message":"Grok Build usage balance exhausted","type":"usage_balance"}}`)
	if !isXAIUsageBalanceExhausted(body) {
		t.Fatal("expected nested error.message to match")
	}
}
