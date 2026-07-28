package authfiles

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// Static catalog merging for discovery results.
//
// For claude, codex and xai the runtime registers the *static* catalog and treats
// upstream discovery as supplementary — the ChatGPT manifest is gated by client
// version, and the Grok CLI gateway advertises a single chat model, so neither is a
// full catalog. That is already documented at the fetch call sites.
//
// The management panel, however, replaced its view with the discovery result. The
// effect was a model list narrower than what the deployment can actually route:
// gpt-image-2 is registered and serviceable for every Codex account, but vanished
// from the panel the moment an operator pressed refresh, which reads as "this
// account does not have that model".
//
// Displayed and routable have to agree, so discovery is merged over the static
// catalog rather than replacing it.

// mergeDiscoveryWithStaticCatalog returns the union of a provider's static catalog
// and its live discovery result.
//
// Upstream wins on conflicting ids: when a provider does report a model, its live
// definition is more current than the compiled-in one. Providers whose discovery is
// authoritative pass through untouched.
func mergeDiscoveryWithStaticCatalog(provider string, live []*registry.ModelInfo) []*registry.ModelInfo {
	if !discoveryIsPartial(provider) {
		return live
	}

	static := staticCatalogForProvider(provider)
	if len(static) == 0 {
		return live
	}

	seen := make(map[string]struct{}, len(live))
	for _, model := range live {
		if model == nil {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
	}

	merged := make([]*registry.ModelInfo, 0, len(live)+len(static))
	merged = append(merged, live...)
	for _, model := range static {
		if model == nil {
			continue
		}
		if _, exists := seen[strings.ToLower(strings.TrimSpace(model.ID))]; exists {
			continue
		}
		merged = append(merged, model)
	}
	return merged
}

// discoveryIsPartial reports whether a provider's upstream listing is known to be a
// subset of what the runtime registers, and therefore must not replace the catalog.
func discoveryIsPartial(provider string) bool {
	return supportsSharedDiscovery(provider)
}

// staticCatalogForProvider resolves the compiled-in catalog the runtime registers
// for a provider, converted to the registry shape the panel renders.
func staticCatalogForProvider(provider string) []*registry.ModelInfo {
	channel := normalizeDiscoveryProvider(provider)
	if channel == "" {
		return nil
	}
	return cloneSDKModelsToRegistry(sdkmodelcatalog.StaticModelDefinitionsByChannel(channel))
}
