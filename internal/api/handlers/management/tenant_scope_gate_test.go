package management

import "testing"

func TestTenantScopedManagementPathIncludesProviderRuntimeRoutes(t *testing.T) {
	for _, path := range []string{
		"/v0/management/gemini-api-key",
		"/v0/management/claude-api-key",
		"/v0/management/bedrock-api-key",
		"/v0/management/opencode-go-api-key/usage",
		"/v0/management/cline-api-key/usage",
		"/v0/management/ollama-cloud-api-key/usage",
		"/v0/management/codex-api-key",
		"/v0/management/vertex-api-key",
		"/v0/management/openai-compatibility",
		"/v0/management/oauth-model-alias",
		"/v0/management/codex-oauth-admission",
		// Auth-file quota preview for tenant-imported OAuth credentials.
		"/v0/management/api-call",
		// Per-account identity fingerprints must stay readable by tenant admins
		// after credentials migrate off the system tenant.
		"/v0/management/identity-fingerprint",
		"/v0/management/identity-fingerprint/account",
		"/v0/management/identity-fingerprint/account/policy",
		"/v0/management/identity-fingerprint/codex/recommendations",
	} {
		if !isTenantScopedManagementPath(path) {
			t.Errorf("isTenantScopedManagementPath(%q) = false", path)
		}
	}
}

func TestTenantScopedManagementPathKeepsGlobalRuntimeRoutesBlocked(t *testing.T) {
	for _, path := range []string{
		"/v0/management/config.yaml",
		"/v0/management/oauth-excluded-models",
		"/v0/management/proxy-url",
		"/v0/management/request-retry",
		"/v0/management/update/apply",
		"/v0/management/logs",
	} {
		if isTenantScopedManagementPath(path) {
			t.Errorf("isTenantScopedManagementPath(%q) = true", path)
		}
	}
}

func TestProviderManagementRoutesUseProviderPermissions(t *testing.T) {
	for _, path := range []string{
		"/v0/management/gemini-api-key",
		"/v0/management/openai-compatibility",
	} {
		if got := permissionForManagementRequest("GET", path); got != "providers.read" {
			t.Errorf("GET %s permission = %q, want providers.read", path, got)
		}
		if got := permissionForManagementRequest("PUT", path); got != "providers.write" {
			t.Errorf("PUT %s permission = %q, want providers.write", path, got)
		}
	}
	for _, path := range []string{
		"/v0/management/api-call",
		"/v0/management/opencode-go-api-key/usage",
	} {
		if got := permissionForManagementRequest("POST", path); got != "providers.test" {
			t.Errorf("POST %s permission = %q, want providers.test", path, got)
		}
	}
}
