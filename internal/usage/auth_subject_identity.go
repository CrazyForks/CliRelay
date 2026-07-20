package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// AuthSubjectIdentity is the single account identity contract used by status,
// usage, quota and identity fingerprints. AccountKey must always equal ID.
// Only provider-stable account_id seeds are eligible for cross-tenant sharing;
// all fallbacks include tenant_id in the seed and remain tenant scoped.
type AuthSubjectIdentity struct {
	ID            string
	Provider      string
	AccountID     string
	Email         string
	SeedKind      string
	SeedHash      string
	SubjectScope  string
	ShareEligible bool
	ShareReason   string
}

type AuthSubjectMatcher struct {
	SubjectID      string
	AuthIndexes    []string
	SourceAliases  []string
	ChannelAliases []string
}

func ResolveAuthSubjectIdentity(auth *coreauth.Auth) *AuthSubjectIdentity {
	if auth == nil {
		return nil
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" {
		provider = "unknown"
	}
	tenantID := coreauth.NormalizedTenantID(auth.TenantID)
	accountID := authMetadataString(auth.Metadata, "account_id", "accountId", "chatgpt_account_id")
	email := strings.ToLower(authEmail(auth))

	seedKind := ""
	seedValue := ""
	shareEligible := false
	subjectScope := "tenant"
	shareReason := "fallback_identity_is_tenant_scoped"
	switch {
	case accountID != "":
		seedKind = "account_id"
		seedValue = accountID
		shareEligible = true
		subjectScope = "shared"
		shareReason = "stable_provider_account_id"
	case email != "":
		seedKind = "email"
		seedValue = email
	case strings.TrimSpace(auth.ID) != "":
		seedKind = "auth_id"
		seedValue = strings.TrimSpace(auth.ID)
	default:
		authIndex := strings.TrimSpace(auth.EnsureIndex())
		if authIndex == "" {
			return nil
		}
		seedKind = "auth_index"
		seedValue = authIndex
	}

	subjectSeed := []string{provider, seedKind, seedValue}
	seedHashValue := seedValue
	if !shareEligible {
		subjectSeed = []string{provider, seedKind, tenantID, seedValue}
		seedHashValue = tenantID + "\x1f" + seedValue
	}

	return &AuthSubjectIdentity{
		ID:            stableAuthSubjectID(subjectSeed...),
		Provider:      provider,
		AccountID:     accountID,
		Email:         email,
		SeedKind:      seedKind,
		SeedHash:      stableSeedHash(seedHashValue),
		SubjectScope:  subjectScope,
		ShareEligible: shareEligible,
		ShareReason:   shareReason,
	}
}

func BuildAuthSubjectMatcher(current *coreauth.Auth, auths []*coreauth.Auth) AuthSubjectMatcher {
	var matcher AuthSubjectMatcher
	if current == nil {
		return matcher
	}

	baseIdentity := ResolveAuthSubjectIdentity(current)
	if baseIdentity != nil {
		matcher.SubjectID = strings.TrimSpace(baseIdentity.ID)
	}

	addAuthIndex := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range matcher.AuthIndexes {
			if existing == value {
				return
			}
		}
		matcher.AuthIndexes = append(matcher.AuthIndexes, value)
	}
	addAlias := func(target *[]string, value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		for _, existing := range *target {
			if existing == value {
				return
			}
		}
		*target = append(*target, value)
	}

	addAuthIndex(current.EnsureIndex())
	if email := authEmail(current); email != "" {
		addAlias(&matcher.SourceAliases, email)
		addAlias(&matcher.ChannelAliases, email)
	}
	if _, accountInfo := current.AccountInfo(); accountInfo != "" {
		addAlias(&matcher.SourceAliases, accountInfo)
	}
	if channelName := current.ChannelName(); channelName != "" {
		addAlias(&matcher.ChannelAliases, channelName)
	}

	if baseIdentity == nil || baseIdentity.ID == "" {
		return matcher
	}

	for _, auth := range auths {
		if auth == nil {
			continue
		}
		identity := ResolveAuthSubjectIdentity(auth)
		if identity == nil || identity.ID != baseIdentity.ID {
			continue
		}
		addAuthIndex(auth.EnsureIndex())
		if email := authEmail(auth); email != "" {
			addAlias(&matcher.SourceAliases, email)
			addAlias(&matcher.ChannelAliases, email)
		}
		if _, accountInfo := auth.AccountInfo(); accountInfo != "" {
			addAlias(&matcher.SourceAliases, accountInfo)
		}
		if channelName := auth.ChannelName(); channelName != "" {
			addAlias(&matcher.ChannelAliases, channelName)
		}
	}

	return matcher
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if email := authMetadataString(auth.Metadata, "email"); email != "" {
		return email
	}
	if auth.Attributes != nil {
		for _, key := range []string{"email", "account_email"} {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func authMetadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		if raw, ok := metadata[key].(string); ok {
			if value := strings.TrimSpace(raw); value != "" {
				return value
			}
		}
	}
	return ""
}

func stableSeedHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

func stableAuthSubjectID(parts ...string) string {
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		normalized = append(normalized, trimmed)
	}
	if len(normalized) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(normalized, "\x1f")))
	return "authsub_" + hex.EncodeToString(sum[:8])
}
