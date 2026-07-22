package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestXAIExecutorProbeQuotaRecovery(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(2 * time.Hour).UTC().Round(time.Second)
	tests := []struct {
		name            string
		usedPercent     int
		wantRecovered   bool
		wantNextRecover bool
	}{
		{name: "recovered", usedPercent: 40, wantRecovered: true},
		{name: "not recovered", usedPercent: 100, wantRecovered: false, wantNextRecover: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatalf("method = %s, want GET", r.Method)
				}
				if r.URL.String() != "https://cli-chat-proxy.grok.com/v1/billing?format=credits" {
					t.Fatalf("url = %s, want Grok weekly billing endpoint", r.URL.String())
				}
				if got := r.Header.Get("Authorization"); got != "Bearer xai-token" {
					t.Fatalf("Authorization = %q", got)
				}
				if got := r.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
					t.Fatalf("%s = %q", xaiTokenAuthHeader, got)
				}
				if got := r.Header.Get("X-UserID"); got != "xai-user" {
					t.Fatalf("X-UserID = %q, want xai-user", got)
				}
				body := fmt.Sprintf(`{"config":{"currentPeriod":{"type":"WEEKLY","end":%q},"creditUsagePercent":%d}}`, resetAt.Format(time.RFC3339), test.usedPercent)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    r,
				}, nil
			}))

			auth := &cliproxyauth.Auth{
				ID:       "xai-auth",
				Provider: "xai",
				Status:   cliproxyauth.StatusActive,
				Attributes: map[string]string{
					"api_key":   "xai-token",
					"using_api": "false",
					"sub":       "xai-user",
				},
			}

			result, err := NewXAIExecutor(nil).ProbeQuotaRecovery(ctx, auth)
			if err != nil {
				t.Fatalf("ProbeQuotaRecovery() error = %v", err)
			}
			if result == nil {
				t.Fatal("ProbeQuotaRecovery() result = nil")
			}
			if result.Recovered != test.wantRecovered {
				t.Fatalf("Recovered = %v, want %v", result.Recovered, test.wantRecovered)
			}
			if test.wantNextRecover {
				if !result.NextRecoverAt.Equal(resetAt) {
					t.Fatalf("NextRecoverAt = %v, want %v", result.NextRecoverAt, resetAt)
				}
			} else if !result.NextRecoverAt.IsZero() {
				t.Fatalf("NextRecoverAt = %v, want zero", result.NextRecoverAt)
			}
		})
	}
}

func TestParseXAIQuotaProbeExhaustedWithoutResetStaysBlocked(t *testing.T) {
	t.Parallel()

	result := parseXAIQuotaProbe([]byte(`{"config":{"currentPeriod":{"type":"WEEKLY"},"creditUsagePercent":100}}`), time.Now())
	if result == nil {
		t.Fatal("parseXAIQuotaProbe() result = nil")
	}
	if result.Recovered {
		t.Fatal("Recovered = true, want false")
	}
	if !result.NextRecoverAt.IsZero() {
		t.Fatalf("NextRecoverAt = %v, want zero without upstream reset", result.NextRecoverAt)
	}
}

var _ cliproxyauth.QuotaRecoveryProber = (*XAIExecutor)(nil)
