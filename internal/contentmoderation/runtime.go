package contentmoderation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const requestDecisionCacheMetadataKey = "content_moderation_decision_cache"

type ProfileResolver interface {
	ResolveProfile(ctx context.Context, tenantID, authFileID, providerKeyID, providerID string) (Profile, string, error)
}

type DecisionEvaluator interface {
	Evaluate(ctx context.Context, profile Profile, input string) Decision
}

type RuntimeModerator struct {
	resolver  ProfileResolver
	evaluator DecisionEvaluator

	lastGoodMu sync.RWMutex
	lastGood   map[resolutionKey]resolvedProfile

	requests  atomic.Uint64
	allows    atomic.Uint64
	blocks    atomic.Uint64
	errors    atomic.Uint64
	cacheHits atomic.Uint64
}

type resolutionKey struct {
	tenantID      string
	authFileID    string
	providerKeyID string
	providerID    string
}

type resolvedProfile struct {
	profile Profile
	source  string
}

type requestDecisionCache struct {
	mu        sync.Mutex
	decisions map[string]Decision
}

type ModerationMetrics struct {
	Requests  uint64 `json:"requests"`
	Allows    uint64 `json:"allows"`
	Blocks    uint64 `json:"blocks"`
	Errors    uint64 `json:"errors"`
	CacheHits uint64 `json:"cache_hits"`
}

func NewRequestModerator(resolver ProfileResolver, evaluator DecisionEvaluator) *RuntimeModerator {
	if evaluator == nil {
		evaluator = NewEvaluator(nil)
	}
	return &RuntimeModerator{
		resolver:  resolver,
		evaluator: evaluator,
		lastGood:  make(map[resolutionKey]resolvedProfile),
	}
}

func (m *RuntimeModerator) Metrics() ModerationMetrics {
	if m == nil {
		return ModerationMetrics{}
	}
	return ModerationMetrics{
		Requests:  m.requests.Load(),
		Allows:    m.allows.Load(),
		Blocks:    m.blocks.Load(),
		Errors:    m.errors.Load(),
		CacheHits: m.cacheHits.Load(),
	}
}

func (m *RuntimeModerator) Moderate(ctx context.Context, auth *coreauth.Auth, opts cliproxyexecutor.Options) coreauth.RequestModerationResult {
	if m == nil || auth == nil || m.resolver == nil || m.evaluator == nil {
		return coreauth.RequestModerationResult{}
	}
	m.requests.Add(1)
	tenantID := coreauth.NormalizedTenantID(metadataString(opts.Metadata, cliproxyexecutor.TenantMetadataKey))
	if coreauth.NormalizedTenantID(auth.TenantID) != tenantID {
		m.recordError(tenantID, auth, "", Profile{}, "tenant_mismatch", 0)
		return coreauth.RequestModerationResult{}
	}

	authFileID, providerKeyID, providerID := runtimeChannelIDs(auth)
	key := resolutionKey{tenantID: tenantID, authFileID: authFileID, providerKeyID: providerKeyID, providerID: providerID}
	resolved, usedLastGood, err := m.resolve(ctx, key)
	if errors.Is(err, ErrNotFound) {
		m.allows.Add(1)
		return coreauth.RequestModerationResult{}
	}
	if err != nil {
		m.recordError(tenantID, auth, "", Profile{}, "store_error", 0)
		return coreauth.RequestModerationResult{}
	}
	if usedLastGood {
		m.recordError(tenantID, auth, resolved.source, resolved.profile, "store_error_last_good", 0)
	}
	profile := resolved.profile
	if profile.Mode == ModeOff {
		m.allows.Add(1)
		m.logDecision(tenantID, auth, resolved.source, profile, Decision{Action: ActionAllow}, false)
		return coreauth.RequestModerationResult{}
	}

	input := ExtractLastUserText(opts.SourceFormat, opts.OriginalRequest)
	if input == "" {
		m.allows.Add(1)
		m.logDecision(tenantID, auth, resolved.source, profile, Decision{Action: ActionAllow}, false)
		return coreauth.RequestModerationResult{}
	}

	cacheKey := decisionCacheKey(profile, input)
	cache := decisionCacheFromMetadata(opts.Metadata)
	if cache != nil {
		if decision, ok := cache.get(cacheKey); ok {
			m.cacheHits.Add(1)
			return m.resultForDecision(tenantID, auth, resolved.source, profile, decision, true)
		}
	}
	decision := m.evaluator.Evaluate(ctx, profile, input)
	if cache != nil {
		cache.put(cacheKey, decision)
	}
	return m.resultForDecision(tenantID, auth, resolved.source, profile, decision, false)
}

func (m *RuntimeModerator) resolve(ctx context.Context, key resolutionKey) (resolvedProfile, bool, error) {
	profile, source, err := m.resolver.ResolveProfile(ctx, key.tenantID, key.authFileID, key.providerKeyID, key.providerID)
	if err == nil {
		resolved := resolvedProfile{profile: profile, source: source}
		m.lastGoodMu.Lock()
		m.lastGood[key] = resolved
		m.lastGoodMu.Unlock()
		return resolved, false, nil
	}
	if errors.Is(err, ErrNotFound) {
		m.lastGoodMu.Lock()
		delete(m.lastGood, key)
		m.lastGoodMu.Unlock()
		return resolvedProfile{}, false, ErrNotFound
	}
	m.lastGoodMu.RLock()
	resolved, ok := m.lastGood[key]
	m.lastGoodMu.RUnlock()
	if ok {
		return resolved, true, nil
	}
	return resolvedProfile{}, false, err
}

func (m *RuntimeModerator) resultForDecision(tenantID string, auth *coreauth.Auth, source string, profile Profile, decision Decision, cached bool) coreauth.RequestModerationResult {
	if decision.Action == ActionAPIError {
		m.allows.Add(1)
		m.recordError(tenantID, auth, source, profile, moderationErrorClass(decision.ModerationError), decision.LatencyMS)
		return coreauth.RequestModerationResult{}
	}
	if decision.WouldBlock {
		m.blocks.Add(1)
		m.logDecision(tenantID, auth, source, profile, decision, cached)
		return coreauth.RequestModerationResult{Blocked: true, Message: profile.BlockMessage, HTTPStatus: profile.BlockHTTPStatus}
	}
	m.allows.Add(1)
	m.logDecision(tenantID, auth, source, profile, decision, cached)
	return coreauth.RequestModerationResult{}
}

func (m *RuntimeModerator) recordError(tenantID string, auth *coreauth.Auth, source string, profile Profile, errorClass string, latencyMS int64) {
	m.errors.Add(1)
	fields := moderationLogFields(tenantID, auth, source, profile)
	fields["action"] = ActionAPIError
	fields["error_class"] = errorClass
	fields["latency_ms"] = latencyMS
	log.WithFields(fields).Warn("content moderation failed open")
}

func (m *RuntimeModerator) logDecision(tenantID string, auth *coreauth.Auth, source string, profile Profile, decision Decision, cached bool) {
	fields := moderationLogFields(tenantID, auth, source, profile)
	fields["action"] = decision.Action
	fields["latency_ms"] = decision.LatencyMS
	fields["cache_hit"] = cached
	if decision.Action == ActionKeywordBlock {
		fields["category"] = "keyword"
		fields["score"] = 1.0
	} else if decision.HighestCategory != "" {
		fields["category"] = decision.HighestCategory
		fields["score"] = decision.HighestScore
	}
	entry := log.WithFields(fields)
	if decision.WouldBlock {
		entry.Warn("content moderation blocked request")
	} else {
		entry.Debug("content moderation allowed request")
	}
}

func moderationLogFields(tenantID string, auth *coreauth.Auth, source string, profile Profile) log.Fields {
	channelType, channelID := resolvedChannel(auth, source)
	return log.Fields{
		"tenant_id":         tenantID,
		"profile_id":        profile.ID,
		"profile_version":   profile.Version,
		"resolution_source": source,
		"channel_type":      channelType,
		"channel_id":        channelID,
	}
}

func resolvedChannel(auth *coreauth.Auth, source string) (string, string) {
	authFileID, providerKeyID, providerID := runtimeChannelIDs(auth)
	switch source {
	case ChannelTypeAuthFile:
		return source, authFileID
	case ChannelTypeProviderKey:
		return source, providerKeyID
	case ChannelTypeProvider:
		return source, providerID
	default:
		return "", ""
	}
}

func runtimeChannelIDs(auth *coreauth.Auth) (authFileID, providerKeyID, providerID string) {
	if auth == nil {
		return "", "", ""
	}
	if strings.TrimSpace(auth.FileName) != "" || strings.TrimSpace(auth.Attributes["path"]) != "" {
		authFileID = strings.TrimSpace(auth.Attributes["gemini_virtual_parent"])
		if authFileID == "" {
			authFileID = strings.TrimSpace(auth.ID)
		}
	}
	providerKeyID = strings.TrimSpace(auth.Attributes["provider_key_id"])
	providerID = strings.TrimSpace(auth.Attributes["provider_config_id"])
	return authFileID, providerKeyID, providerID
}

func decisionCacheKey(profile Profile, input string) string {
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%s:%d:%x", profile.ID, profile.Version, hash)
}

func decisionCacheFromMetadata(metadata map[string]any) *requestDecisionCache {
	if metadata == nil {
		return nil
	}
	if cache, ok := metadata[requestDecisionCacheMetadataKey].(*requestDecisionCache); ok && cache != nil {
		return cache
	}
	cache := &requestDecisionCache{decisions: make(map[string]Decision)}
	metadata[requestDecisionCacheMetadataKey] = cache
	return cache
}

func (c *requestDecisionCache) get(key string) (Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	decision, ok := c.decisions[key]
	return decision, ok
}

func (c *requestDecisionCache) put(key string, decision Decision) {
	c.mu.Lock()
	c.decisions[key] = decision
	c.mu.Unlock()
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	switch value := metadata[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func moderationErrorClass(message string) string {
	message = strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(message, "deadline exceeded") || strings.Contains(message, "timeout"):
		return "timeout"
	case strings.Contains(message, "returned status"):
		return "upstream_status"
	case strings.Contains(message, "decode") || strings.Contains(message, "missing category scores"):
		return "invalid_response"
	case strings.Contains(message, "request failed"):
		return "transport"
	default:
		return "moderation_error"
	}
}
