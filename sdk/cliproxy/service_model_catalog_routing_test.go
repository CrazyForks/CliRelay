package cliproxy

import "testing"

func modelIDs(models []*ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		if model != nil {
			out = append(out, model.ID)
		}
	}
	return out
}

func TestAppendOAuthProviderModelConfigsUsesTenantOwnerMapping(t *testing.T) {
	// Operators attach catalog models to a channel through the tenant's
	// auth-group→owner mapping (e.g. xai → grok-build). Before, only the built-in
	// provider aliases counted, so a freshly added model id stayed unroutable and
	// the request never reached the upstream.
	rows := []oauthProviderModelConfigRow{
		{ModelID: "grok-4.7", OwnedBy: "grok-build", Source: "seed", Enabled: true},
		{ModelID: "unrelated-model", OwnedBy: "some-other-owner", Source: "user", Enabled: true},
	}
	live := []*ModelInfo{{ID: "grok-4.5", Object: "model", OwnedBy: "xai"}}

	got := appendOAuthProviderModelConfigs(live, "xai", "oauth", rows, []string{"grok-build"})

	if !hasModelID(got, "grok-4.7") {
		t.Fatalf("models = %v, want mapped-owner catalog row grok-4.7", modelIDs(got))
	}
	if !hasModelID(got, "grok-4.5") {
		t.Fatalf("models = %v, want live model kept", modelIDs(got))
	}
	if hasModelID(got, "unrelated-model") {
		t.Fatalf("models = %v, must not include unmapped owner", modelIDs(got))
	}
}

func TestAppendOAuthProviderModelConfigsWithoutMappingKeepsProviderAliases(t *testing.T) {
	// Without a tenant mapping the built-in aliases must still work, otherwise the
	// existing claude/codex behaviour would regress.
	rows := []oauthProviderModelConfigRow{
		{ModelID: "claude-catalog-model", OwnedBy: "anthropic", Source: "user", Enabled: true},
		{ModelID: "grok-4.7", OwnedBy: "grok-build", Source: "seed", Enabled: true},
	}

	got := appendOAuthProviderModelConfigs(nil, "claude", "oauth", rows, nil)

	if !hasModelID(got, "claude-catalog-model") {
		t.Fatalf("models = %v, want alias-owned catalog row", modelIDs(got))
	}
	if hasModelID(got, "grok-4.7") {
		t.Fatalf("models = %v, must not pull in another owner's rows", modelIDs(got))
	}
}

func TestAppendOAuthProviderModelConfigsSkipsDisabledAndAPIKeyAuth(t *testing.T) {
	rows := []oauthProviderModelConfigRow{
		{ModelID: "disabled-model", OwnedBy: "grok-build", Source: "seed", Enabled: false},
		{ModelID: "legacy-priced-model", OwnedBy: "grok-build", Source: "legacy-pricing", Enabled: true},
		{ModelID: "grok-4.7", OwnedBy: "grok-build", Source: "seed", Enabled: true},
	}
	mapped := []string{"grok-build"}

	got := appendOAuthProviderModelConfigs(nil, "xai", "oauth", rows, mapped)
	if hasModelID(got, "disabled-model") {
		t.Fatalf("models = %v, must not include disabled row", modelIDs(got))
	}
	// Only operator-authored sources are routable; pricing-only bookkeeping rows
	// are not a statement that the upstream serves the model.
	if hasModelID(got, "legacy-priced-model") {
		t.Fatalf("models = %v, must not include non-catalog source", modelIDs(got))
	}
	if !hasModelID(got, "grok-4.7") {
		t.Fatalf("models = %v, want grok-4.7", modelIDs(got))
	}

	if apikey := appendOAuthProviderModelConfigs(nil, "xai", "apikey", rows, mapped); len(apikey) != 0 {
		t.Fatalf("api-key auth models = %v, want none (config block owns that list)", modelIDs(apikey))
	}
}
