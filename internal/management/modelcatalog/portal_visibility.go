package modelcatalog

import (
	"net/http"
	"strings"
)

const portalRootModelsPath = "/v1/models"

// PortalVisibleModelIDs returns the model IDs shared by configured availability
// and the root OpenAI-compatible GET /v1/models path. The management model
// plaza derives its tenant-visible catalog from these same two sources.
func (s *Service) PortalVisibleModelIDs(allowedChannelsRaw, allowedGroupsRaw string) map[string]struct{} {
	configuredIDs := configuredAvailabilityModelIDs(s.ConfiguredAvailability(allowedChannelsRaw, allowedGroupsRaw))
	rootPathIDs := rootModelsPathIDs(s.PathAvailability())
	visible := make(map[string]struct{})
	for id := range configuredIDs {
		if _, ok := rootPathIDs[id]; ok {
			visible[id] = struct{}{}
		}
	}
	return visible
}

func configuredAvailabilityModelIDs(availability map[string]any) map[string]struct{} {
	ids := make(map[string]struct{})
	data, _ := availability["data"].([]map[string]any)
	for _, model := range data {
		if id := normalizedModelID(modelPathStringValue(model["id"])); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func rootModelsPathIDs(availability map[string]any) map[string]struct{} {
	ids := make(map[string]struct{})
	data, _ := availability["data"].([]modelPathAvailabilityResponse)
	for _, model := range data {
		for _, path := range model.Paths {
			if path.Scope == "root" && path.Method == http.MethodGet && path.Path == portalRootModelsPath {
				if id := normalizedModelID(model.ID); id != "" {
					ids[id] = struct{}{}
				}
				break
			}
		}
	}
	return ids
}

func normalizedModelID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}
