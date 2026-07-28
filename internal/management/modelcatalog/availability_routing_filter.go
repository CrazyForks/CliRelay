package modelcatalog

import (
	"strings"

	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
)

// filterModelsByRoutingAllowedModels drops models outside the effective channel
// group's allowed-models list. Used after live-discovery merge: discovery models
// are not registry-backed, so CanServe cannot enforce AllowedModels for them.
// filterModelsByRoutingAllowedModelsOpts skips the filter entirely when the caller
// is the channel-group editor, which must list models it is meant to add.
func (s *Service) filterModelsByRoutingAllowedModelsOpts(opts AvailabilityFilterOptions, models []map[string]any, allowedGroupsRaw string) []map[string]any {
	if opts.IgnoreGroupAllowedModels {
		return models
	}
	return s.filterModelsByRoutingAllowedModels(models, allowedGroupsRaw)
}

func (s *Service) filterModelsByRoutingAllowedModels(models []map[string]any, allowedGroupsRaw string) []map[string]any {
	allowed := s.routingAllowedModels(allowedGroupsRaw)
	if allowed == nil {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		if routingAllowedModelMatches(id, allowed) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// routingAllowedModels returns the union of AllowedModels for the scoped groups.
// nil means unrestricted (empty AllowedModels on any matched group, or no groups).
func (s *Service) routingAllowedModels(allowedGroupsRaw string) []string {
	if s == nil {
		return nil
	}
	routing := tenantRoutingConfig(s.tenantID, s.cfg)
	if routing == nil {
		return nil
	}
	scopedGroups := internalrouting.ParseNormalizedSet(strings.TrimSpace(allowedGroupsRaw), internalrouting.NormalizeGroupName)
	if len(scopedGroups) == 0 {
		if routing.IncludeDefaultGroup {
			scopedGroups = map[string]struct{}{"default": {}}
		}
	}
	if len(scopedGroups) == 0 {
		return nil
	}
	var allowedModels []string
	matched := false
	for _, group := range routing.ChannelGroups {
		groupName := internalrouting.NormalizeGroupName(group.Name)
		if _, ok := scopedGroups[groupName]; !ok {
			continue
		}
		matched = true
		// Empty AllowedModels means the group can use every model its channels serve.
		if len(group.AllowedModels) == 0 {
			return nil
		}
		allowedModels = append(allowedModels, group.AllowedModels...)
	}
	if !matched || len(allowedModels) == 0 {
		return nil
	}
	return allowedModels
}

func routingAllowedModelMatches(model string, allowedModels []string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, allowed := range allowedModels {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		if strings.EqualFold(model, allowed) {
			return true
		}
		if idx := strings.Index(model, "/"); idx >= 0 && strings.EqualFold(strings.TrimSpace(model[idx+1:]), allowed) {
			return true
		}
	}
	return false
}
