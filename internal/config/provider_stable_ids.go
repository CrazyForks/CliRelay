package config

import (
	"strings"

	"github.com/google/uuid"
)

const providerStableIDMigrationVersion = 1

// migrateProviderStableIDsV1 is kept versioned so future identifier migrations can
// be introduced without changing the persisted IDs created by this rollout.
func migrateProviderStableIDsV1(cfg *Config) bool {
	_ = providerStableIDMigrationVersion
	return cfg.EnsureProviderStableIDs()
}

// EnsureProviderStableIDs assigns persistent UUIDs to every config-backed provider channel.
// Existing valid IDs are never recomputed, so credential rotation does not move bindings.
func (cfg *Config) EnsureProviderStableIDs() bool {
	if cfg == nil {
		return false
	}
	changed := false
	seen := make(map[string]struct{})
	ensure := func(id *string) {
		trimmed := strings.TrimSpace(*id)
		if parsed, err := uuid.Parse(trimmed); err == nil {
			normalized := parsed.String()
			if _, exists := seen[normalized]; !exists {
				seen[normalized] = struct{}{}
				if *id != normalized {
					*id = normalized
					changed = true
				}
				return
			}
		}
		*id = uuid.NewString()
		seen[*id] = struct{}{}
		changed = true
	}

	for i := range cfg.GeminiKey {
		ensure(&cfg.GeminiKey[i].ID)
	}
	for i := range cfg.CodexKey {
		ensure(&cfg.CodexKey[i].ID)
	}
	for i := range cfg.ClaudeKey {
		ensure(&cfg.ClaudeKey[i].ID)
	}
	for i := range cfg.BedrockKey {
		ensure(&cfg.BedrockKey[i].ID)
	}
	for i := range cfg.OpenCodeGoKey {
		ensure(&cfg.OpenCodeGoKey[i].ID)
	}
	for i := range cfg.ClineKey {
		ensure(&cfg.ClineKey[i].ID)
	}
	for i := range cfg.OllamaCloudKey {
		ensure(&cfg.OllamaCloudKey[i].ID)
	}
	for i := range cfg.OpenAICompatibility {
		ensure(&cfg.OpenAICompatibility[i].ID)
		for j := range cfg.OpenAICompatibility[i].APIKeyEntries {
			ensure(&cfg.OpenAICompatibility[i].APIKeyEntries[j].ID)
		}
	}
	for i := range cfg.VertexCompatAPIKey {
		ensure(&cfg.VertexCompatAPIKey[i].ID)
	}
	return changed
}

// PreserveMissingProviderStableIDs keeps IDs across full-array management PUTs whose
// clients predate the ID field. Explicit IDs from import/export remain authoritative.
func PreserveMissingProviderStableIDs(previous, next *Config) {
	if previous == nil || next == nil {
		return
	}
	preserveIDs(previous.GeminiKey, next.GeminiKey, func(v *GeminiKey) *string { return &v.ID })
	preserveIDs(previous.CodexKey, next.CodexKey, func(v *CodexKey) *string { return &v.ID })
	preserveIDs(previous.ClaudeKey, next.ClaudeKey, func(v *ClaudeKey) *string { return &v.ID })
	preserveIDs(previous.BedrockKey, next.BedrockKey, func(v *BedrockKey) *string { return &v.ID })
	preserveIDs(previous.OpenCodeGoKey, next.OpenCodeGoKey, func(v *OpenCodeGoKey) *string { return &v.ID })
	preserveIDs(previous.ClineKey, next.ClineKey, func(v *ClineKey) *string { return &v.ID })
	preserveIDs(previous.OllamaCloudKey, next.OllamaCloudKey, func(v *OllamaCloudKey) *string { return &v.ID })
	preserveIDs(previous.VertexCompatAPIKey, next.VertexCompatAPIKey, func(v *VertexCompatKey) *string { return &v.ID })
	preserveIDs(previous.OpenAICompatibility, next.OpenAICompatibility, func(v *OpenAICompatibility) *string { return &v.ID })
	for i := range next.OpenAICompatibility {
		if i >= len(previous.OpenAICompatibility) {
			break
		}
		preserveIDs(
			previous.OpenAICompatibility[i].APIKeyEntries,
			next.OpenAICompatibility[i].APIKeyEntries,
			func(v *OpenAICompatibilityAPIKey) *string { return &v.ID },
		)
	}
}

func preserveIDs[T any](previous, next []T, id func(*T) *string) {
	for i := range next {
		if strings.TrimSpace(*id(&next[i])) != "" || i >= len(previous) {
			continue
		}
		*id(&next[i]) = strings.TrimSpace(*id(&previous[i]))
	}
}
