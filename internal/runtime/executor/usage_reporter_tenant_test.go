package executor

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type tenantUsageCapturePlugin struct {
	records chan coreusage.Record
}

func (p *tenantUsageCapturePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	select {
	case p.records <- record:
	default:
	}
}

func TestUsageReporterPublishesTrustedTenantSeparatelyFromAPIKeyLabel(t *testing.T) {
	const (
		tenantID     = "00000000-0000-0000-0000-00000000000a"
		systemAPIKey = "POST /image-generation/test"
		model        = "image-generation-trusted-tenant-test"
	)
	plugin := &tenantUsageCapturePlugin{records: make(chan coreusage.Record, 8)}
	coreusage.RegisterPlugin(plugin)

	ctx := context.WithValue(context.Background(), util.ContextKeyTrustedTenantID, tenantID)
	ctx = context.WithValue(ctx, util.ContextKeyAPIKey, systemAPIKey)
	reporter := newUsageReporter(ctx, "codex", model, "", nil)
	reporter.setThinkingLevel("high")
	reporter.ensurePublished(ctx)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case record := <-plugin.records:
			if record.Model != model {
				continue
			}
			if record.TrustedTenantID != tenantID {
				t.Fatalf("TrustedTenantID = %q, want %q", record.TrustedTenantID, tenantID)
			}
			if record.APIKey != systemAPIKey {
				t.Fatalf("APIKey = %q, want display label %q", record.APIKey, systemAPIKey)
			}
			if record.ThinkingLevel != "high" {
				t.Fatalf("ThinkingLevel = %q, want high", record.ThinkingLevel)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for usage record")
		}
	}
}
