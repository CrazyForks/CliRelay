package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type retryAfterQuotaErrorStub struct {
	message      string
	status       int
	quotaWindow  string
	quotaMinutes int
	retryAfter   time.Duration
}

func (e *retryAfterQuotaErrorStub) Error() string { return e.message }
func (e *retryAfterQuotaErrorStub) StatusCode() int {
	return e.status
}
func (e *retryAfterQuotaErrorStub) QuotaWindow() (string, int) {
	return e.quotaWindow, e.quotaMinutes
}
func (e *retryAfterQuotaErrorStub) RetryAfter() *time.Duration {
	if e.retryAfter <= 0 {
		return nil
	}
	d := e.retryAfter
	return &d
}

type xaiQuotaExecutor struct {
	err error
}

func (e *xaiQuotaExecutor) Identifier() string { return "xai" }
func (e *xaiQuotaExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.err
}
func (e *xaiQuotaExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.err
}
func (e *xaiQuotaExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }
func (e *xaiQuotaExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.err
}
func (e *xaiQuotaExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, e.err
}

func TestManagerMarkResult_XAI402BalanceExhaustedUsesExplicitRetryAfter(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "grok-4.5"
	retryAfter := 47 * time.Hour
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &retryAfterQuotaErrorStub{
			message:      `{"error":"Grok Build usage balance exhausted"}`,
			status:       http.StatusPaymentRequired,
			quotaWindow:  "week",
			quotaMinutes: 10080,
			retryAfter:   retryAfter,
		},
	})
	auth := &Auth{
		ID:       "xai-weekly",
		Provider: "xai",
		Status:   StatusActive,
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	before := time.Now()
	_, err := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 402")
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if !state.Quota.Exceeded || state.Quota.Reason != "quota" {
		t.Fatalf("quota = %#v, want exceeded quota", state.Quota)
	}
	if state.Quota.Window != "week" || state.Quota.WindowMinutes != 10080 {
		t.Fatalf("quota window = %q/%d, want week/10080", state.Quota.Window, state.Quota.WindowMinutes)
	}
	minExpected := before.Add(retryAfter - time.Minute)
	maxExpected := before.Add(retryAfter + time.Minute)
	if state.NextRetryAfter.Before(minExpected) || state.NextRetryAfter.After(maxExpected) {
		t.Fatalf("NextRetryAfter = %v, want ~%v", state.NextRetryAfter, before.Add(retryAfter))
	}
	if !state.Quota.NextRecoverAt.Equal(state.NextRetryAfter) {
		t.Fatalf("NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, state.NextRetryAfter)
	}
}

func TestManagerMarkResult_XAI402BalanceExhaustedWithoutRetryDoesNotUseWeekLength(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "grok-4.5"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &retryAfterQuotaErrorStub{
			message:      `{"error":"Grok Build usage balance exhausted"}`,
			status:       http.StatusPaymentRequired,
			quotaWindow:  "week",
			quotaMinutes: 10080,
		},
	})
	auth := &Auth{
		ID:       "xai-weekly-no-reset",
		Provider: "xai",
		Status:   StatusActive,
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	before := time.Now()
	_, err := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 402")
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if !state.Quota.Exceeded || state.Quota.Window != "week" {
		t.Fatalf("quota = %#v, want week exceeded", state.Quota)
	}
	// Must not treat WindowMinutes(10080) as remaining cooldown (~7d).
	if state.NextRetryAfter.After(before.Add(time.Hour)) {
		t.Fatalf("NextRetryAfter = %v, want short probe backoff not week length", state.NextRetryAfter)
	}
	if state.NextRetryAfter.Before(before) {
		t.Fatalf("NextRetryAfter = %v, want after now", state.NextRetryAfter)
	}
}

func TestManagerMarkResult_Generic402KeepsThirtyMinuteCooldown(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "grok-4.5"
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(&xaiQuotaExecutor{
		err: &statusQuotaErrorStub{
			message: "payment required",
			status:  http.StatusPaymentRequired,
		},
	})
	auth := &Auth{
		ID:       "xai-generic-402",
		Provider: "xai",
		Status:   StatusActive,
	}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	before := time.Now()
	_, err := manager.Execute(ctx, []string{"xai"}, cliproxyexecutor.Request{
		Model: model,
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SinglePickMetadataKey: true,
		},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want upstream 402")
	}

	got, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("updated auth missing")
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatal("model state missing")
	}
	if state.Quota.Exceeded {
		t.Fatalf("quota exceeded = true for generic 402, want payment_required path")
	}
	if state.NextRetryAfter.Before(before.Add(29*time.Minute)) || state.NextRetryAfter.After(before.Add(31*time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want ~30m payment_required", state.NextRetryAfter)
	}
}

func TestApplyAuthFailureState_XAI402BalanceExhausted(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	retry := 47 * time.Hour
	auth := &Auth{ID: "auth", Provider: "xai", Status: StatusActive}
	applyAuthFailureState(auth, &Error{
		Message:            `{"error":"Grok Build usage balance exhausted"}`,
		HTTPStatus:         http.StatusPaymentRequired,
		QuotaWindow:        "week",
		QuotaWindowMinutes: 10080,
	}, &retry, now)

	if !auth.Quota.Exceeded || auth.Quota.Window != "week" || auth.Quota.WindowMinutes != 10080 {
		t.Fatalf("quota = %#v, want week exceeded", auth.Quota)
	}
	if !auth.NextRetryAfter.Equal(now.Add(retry)) {
		t.Fatalf("NextRetryAfter = %v, want %v", auth.NextRetryAfter, now.Add(retry))
	}
	if auth.StatusMessage == "payment_required" {
		t.Fatal("StatusMessage still payment_required, want balance exhausted / quota path")
	}
}

func TestApplyAuthFailureState_XAI402WithoutRetryDoesNotUseWindowMinutes(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	auth := &Auth{ID: "auth", Provider: "xai", Status: StatusActive}
	applyAuthFailureState(auth, &Error{
		Message:            `{"error":"Grok Build usage balance exhausted"}`,
		HTTPStatus:         http.StatusPaymentRequired,
		QuotaWindow:        "week",
		QuotaWindowMinutes: 10080,
	}, nil, now)

	if !auth.Quota.Exceeded || auth.Quota.Window != "week" {
		t.Fatalf("quota = %#v, want week exceeded", auth.Quota)
	}
	if auth.NextRetryAfter.Sub(now) >= time.Hour {
		t.Fatalf("NextRetryAfter = %v, want short probe backoff not WindowMinutes", auth.NextRetryAfter)
	}
}
