package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type publicUsageSubject struct {
	TenantID  string
	EndUserID string
	APIKey    string
	APIKeyRow *usage.APIKeyRow
}

func (s publicUsageSubject) logQueryParams(days int) usage.LogQueryParams {
	params := usage.LogQueryParams{TenantID: s.TenantID, Days: days}
	if strings.TrimSpace(s.EndUserID) != "" {
		params.EndUserID = s.EndUserID
	} else if strings.TrimSpace(s.APIKey) != "" {
		params.APIKeys = []string{s.APIKey}
	}
	return params
}

// resolvePublicUsageSubject binds portal requests to the authenticated end user.
// Raw-secret callers remain supported for CC Switch and legacy public lookup.
func (h *Handler) resolvePublicUsageSubject(c *gin.Context, apiKey string) (publicUsageSubject, bool) {
	if token := bearerToken(c); strings.HasPrefix(token, "cpt_") {
		user, _, ok := h.authenticatePortal(c)
		if !ok {
			return publicUsageSubject{}, false
		}
		return publicUsageSubject{
			TenantID:  strings.TrimSpace(user.TenantID),
			EndUserID: strings.TrimSpace(user.ID),
		}, true
	}

	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key parameter is required"})
		return publicUsageSubject{}, false
	}

	subject := publicUsageSubject{APIKey: apiKey}
	if row := usage.GetAPIKey(apiKey); row != nil {
		subject.APIKeyRow = row
		subject.TenantID = strings.TrimSpace(row.TenantID)
		subject.EndUserID = strings.TrimSpace(row.EndUserID)
	} else {
		subject.TenantID = usage.ResolveAPIKeyTenant(apiKey)
	}
	return subject, true
}
