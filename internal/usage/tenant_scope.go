package usage

import "strings"

const systemTenantID = "00000000-0000-0000-0000-000000000001"

func normalizeTenantID(tenantID string) string {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return systemTenantID
	}
	return tenantID
}

func resolveRequestLogTenantID(trustedTenantID, apiKey string) string {
	if tenantID := strings.TrimSpace(trustedTenantID); tenantID != "" {
		return normalizeTenantID(tenantID)
	}
	return normalizeTenantID(ResolveAPIKeyTenant(apiKey))
}

// isSystemTenant reports whether tenantID resolves to the shared system catalog tenant.
// OpenRouter pricing/model metadata is written there and inherited by business tenants on read.
func isSystemTenant(tenantID string) bool {
	return normalizeTenantID(tenantID) == systemTenantID
}
