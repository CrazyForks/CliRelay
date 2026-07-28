package authfiles

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func mergedIDs(provider string, live []*registry.ModelInfo) map[string]struct{} {
	merged := mergeDiscoveryWithStaticCatalog(provider, live)
	ids := make(map[string]struct{}, len(merged))
	for _, model := range merged {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}

// TestRefreshKeepsModelsDiscoveryOmits is the reported bug: pressing refresh on a
// Codex account dropped gpt-image-2 from the panel, because the ChatGPT manifest is
// a subset of what the runtime actually registers. The panel then showed a narrower
// list than the deployment can route, which reads as "this account lacks the model".
func TestRefreshKeepsModelsDiscoveryOmits(t *testing.T) {
	live := []*registry.ModelInfo{{ID: "gpt-5.5", Object: "model"}}

	ids := mergedIDs("codex", live)
	if _, ok := ids["gpt-5.5"]; !ok {
		t.Error("the discovered model was lost")
	}
	if _, ok := ids["gpt-image-2"]; !ok {
		t.Error("gpt-image-2 is registered and routable but missing from the panel view")
	}
}

// TestUpstreamDefinitionWins keeps discovery authoritative for anything it reports,
// so a live definition is never shadowed by the compiled-in one.
func TestUpstreamDefinitionWins(t *testing.T) {
	live := []*registry.ModelInfo{{ID: "gpt-image-2", Object: "model", DisplayName: "Live Name"}}

	merged := mergeDiscoveryWithStaticCatalog("codex", live)
	count := 0
	var found *registry.ModelInfo
	for _, model := range merged {
		if model != nil && model.ID == "gpt-image-2" {
			count++
			found = model
		}
	}
	if count != 1 {
		t.Fatalf("gpt-image-2 appears %d times, want 1", count)
	}
	if found.DisplayName != "Live Name" {
		t.Error("the static definition shadowed what upstream reported")
	}
}

// TestAuthoritativeProvidersAreUntouched: antigravity discovery replaces the
// registry, so merging a static catalog into it would resurrect stale models.
func TestAuthoritativeProvidersAreUntouched(t *testing.T) {
	live := []*registry.ModelInfo{{ID: "only-model", Object: "model"}}

	merged := mergeDiscoveryWithStaticCatalog("antigravity", live)
	if len(merged) != 1 || merged[0].ID != "only-model" {
		t.Errorf("merged = %v, want the discovery result unchanged", merged)
	}
}

func TestXAIRefreshKeepsImageModels(t *testing.T) {
	// The Grok CLI gateway advertises a single chat model; the Imagine models are
	// not discoverable and must still be listed.
	ids := mergedIDs("xai", []*registry.ModelInfo{{ID: "grok-4.5", Object: "model"}})
	for _, modelID := range []string{"grok-4.5", "grok-imagine-image"} {
		if _, ok := ids[modelID]; !ok {
			t.Errorf("%s missing from the merged xai list", modelID)
		}
	}
}
