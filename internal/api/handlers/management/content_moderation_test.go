package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func setupContentModerationHandlerTest(t *testing.T) (*Handler, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	const tenantID = "00000000-0000-0000-0000-0000000000ab"
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-file-1",
		TenantID: tenantID,
		FileName: "auth-file-1.json",
		Provider: "codex",
		Label:    "Codex Account",
		Status:   coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	cfg := &config.Config{GeminiKey: []config.GeminiKey{{ID: "11111111-1111-4111-8111-111111111111", Name: "Gemini Provider", APIKey: "provider-secret"}}}
	if err := usage.UpsertRuntimeSettingForTenant(tenantID, usage.RuntimeSettingGeminiKeys, cfg.GeminiKey); err != nil {
		t.Fatalf("persist tenant provider: %v", err)
	}
	return &Handler{cfg: cfg, authManager: manager}, tenantID
}

func moderationContext(tenantID, method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
	c.Request = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

type managementMetricsResolver struct {
	profile contentmoderation.Profile
}

func (r managementMetricsResolver) ResolveProfile(context.Context, string, string, string, string) (contentmoderation.Profile, string, error) {
	return r.profile, contentmoderation.ChannelTypeProviderKey, nil
}

type managementMetricsEvaluator struct{}

func (managementMetricsEvaluator) Evaluate(context.Context, contentmoderation.Profile, string) contentmoderation.Decision {
	return contentmoderation.Decision{Action: contentmoderation.ActionAllow}
}

func TestContentModerationMetricsUseLiveRuntime(t *testing.T) {
	const tenantID = "tenant-metrics"
	profile, err := contentmoderation.NewProfile(tenantID, "profile-metrics", contentmoderation.CreateProfileInput{
		Name:        "metrics",
		Mode:        contentmoderation.ModePreBlock,
		KeywordMode: contentmoderation.KeywordModeKeywordOnly,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	runtime := contentmoderation.NewRequestModerator(managementMetricsResolver{profile: profile}, managementMetricsEvaluator{})
	previous := contentmoderation.Runtime()
	contentmoderation.SetRuntime(runtime)
	t.Cleanup(func() { contentmoderation.SetRuntime(previous) })
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": "safe"}}})
	runtime.Moderate(context.Background(), &coreauth.Auth{
		ID:       "auth-metrics",
		TenantID: tenantID,
		Attributes: map[string]string{
			"provider_key_id": "provider-key-metrics",
		},
	}, cliproxyexecutor.Options{
		OriginalRequest: body,
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID},
	})

	h := &Handler{}
	c, rec := moderationContext(tenantID, http.MethodGet, "/v0/management/content-moderation/metrics", "")
	h.GetContentModerationMetrics(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	var metrics contentmoderation.ModerationMetrics
	if err = json.Unmarshal(rec.Body.Bytes(), &metrics); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	if metrics.Requests != 1 || metrics.Allows != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestContentModerationMetricsAreTenantScoped(t *testing.T) {
	const tenantA = "tenant-metrics-a"
	const tenantB = "tenant-metrics-b"
	profileA, err := contentmoderation.NewProfile(tenantA, "profile-a", contentmoderation.CreateProfileInput{
		Name:        "a",
		Mode:        contentmoderation.ModePreBlock,
		KeywordMode: contentmoderation.KeywordModeKeywordOnly,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile A: %v", err)
	}
	profileB, err := contentmoderation.NewProfile(tenantB, "profile-b", contentmoderation.CreateProfileInput{
		Name:        "b",
		Mode:        contentmoderation.ModePreBlock,
		KeywordMode: contentmoderation.KeywordModeKeywordOnly,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile B: %v", err)
	}
	runtime := contentmoderation.NewRequestModerator(tenantAwareMetricsResolver{
		profiles: map[string]contentmoderation.Profile{
			tenantA: profileA,
			tenantB: profileB,
		},
	}, managementMetricsEvaluator{})
	previous := contentmoderation.Runtime()
	contentmoderation.SetRuntime(runtime)
	t.Cleanup(func() { contentmoderation.SetRuntime(previous) })

	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": "safe"}}})
	for i := 0; i < 3; i++ {
		runtime.Moderate(context.Background(), &coreauth.Auth{
			ID: "auth-a", TenantID: tenantA, Attributes: map[string]string{"provider_key_id": "key-a"},
		}, cliproxyexecutor.Options{
			OriginalRequest: body, SourceFormat: sdktranslator.FormatOpenAI,
			Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenantA},
		})
	}
	runtime.Moderate(context.Background(), &coreauth.Auth{
		ID: "auth-b", TenantID: tenantB, Attributes: map[string]string{"provider_key_id": "key-b"},
	}, cliproxyexecutor.Options{
		OriginalRequest: body, SourceFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenantB},
	})

	h := &Handler{}
	cA, recA := moderationContext(tenantA, http.MethodGet, "/v0/management/content-moderation/metrics", "")
	h.GetContentModerationMetrics(cA)
	cB, recB := moderationContext(tenantB, http.MethodGet, "/v0/management/content-moderation/metrics", "")
	h.GetContentModerationMetrics(cB)

	var metricsA, metricsB contentmoderation.ModerationMetrics
	if err := json.Unmarshal(recA.Body.Bytes(), &metricsA); err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if err := json.Unmarshal(recB.Body.Bytes(), &metricsB); err != nil {
		t.Fatalf("decode B: %v", err)
	}
	if metricsA.Requests != 3 || metricsA.Allows != 3 {
		t.Fatalf("tenant A metrics = %#v, want requests/allows=3", metricsA)
	}
	if metricsB.Requests != 1 || metricsB.Allows != 1 {
		t.Fatalf("tenant B metrics = %#v, want requests/allows=1", metricsB)
	}
}

type tenantAwareMetricsResolver struct {
	profiles map[string]contentmoderation.Profile
}

func (r tenantAwareMetricsResolver) ResolveProfile(_ context.Context, tenantID, _, _, _ string) (contentmoderation.Profile, string, error) {
	profile, ok := r.profiles[tenantID]
	if !ok {
		return contentmoderation.Profile{}, "", contentmoderation.ErrNotFound
	}
	return profile, contentmoderation.ChannelTypeProviderKey, nil
}

func TestContentModerationProfileUsesPrincipalTenantAndHidesSecret(t *testing.T) {
	h, tenantID := setupContentModerationHandlerTest(t)
	body := `{"tenant_id":"attacker-tenant","name":"primary","mode":"pre_block","keyword_mode":"api_only","api_key":"moderation-secret"}`
	c, rec := moderationContext(tenantID, http.MethodPost, "/v0/management/content-moderation/profiles", body)
	h.PostContentModerationProfile(c)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "moderation-secret") || strings.Contains(rec.Body.String(), "attacker-tenant") {
		t.Fatalf("response leaked secret/body tenant: %s", rec.Body.String())
	}
	profiles, err := h.contentModerationStore().ListProfiles(context.Background(), tenantID)
	if err != nil || len(profiles) != 1 || profiles[0].TenantID != tenantID {
		t.Fatalf("stored profiles = %#v err=%v", profiles, err)
	}
	other, err := h.contentModerationStore().ListProfiles(context.Background(), "attacker-tenant")
	if err != nil || len(other) != 0 {
		t.Fatalf("attacker tenant profiles = %#v err=%v", other, err)
	}
}

func TestContentModerationProfileTestEvaluatesDisabledProfile(t *testing.T) {
	h, tenantID := setupContentModerationHandlerTest(t)
	profile, err := contentmoderation.NewProfile(tenantID, "profile-disabled-test", contentmoderation.CreateProfileInput{
		Name:            "disabled test",
		Mode:            contentmoderation.ModeOff,
		KeywordMode:     contentmoderation.KeywordModeKeywordOnly,
		BlockedKeywords: []string{"blocked"},
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err = h.contentModerationStore().CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	c, rec := moderationContext(tenantID, http.MethodPost, "/v0/management/content-moderation/profiles/"+profile.ID+"/test", `{"input":"contains BLOCKED text"}`)
	c.Params = gin.Params{{Key: "id", Value: profile.ID}}
	h.PostContentModerationProfileTest(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST test status=%d body=%s", rec.Code, rec.Body.String())
	}
	var decision contentmoderation.Decision
	if err = json.Unmarshal(rec.Body.Bytes(), &decision); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if !decision.WouldBlock || decision.Action != contentmoderation.ActionKeywordBlock || decision.MatchedKeyword != "blocked" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestContentModerationChannelsArePagedAndSecretFree(t *testing.T) {
	h, tenantID := setupContentModerationHandlerTest(t)
	c, rec := moderationContext(tenantID, http.MethodGet, "/v0/management/content-moderation/channels?page=1&page_size=1", "")
	h.GetContentModerationChannels(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET channels status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "provider-secret") {
		t.Fatalf("channel response leaked provider secret: %s", rec.Body.String())
	}
	var page contentmoderation.ChannelPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if page.PageSize != 1 || len(page.Items) != 1 || page.Total != 2 {
		t.Fatalf("page = %#v", page)
	}
}

func TestProviderDeleteRemovesContentModerationBindings(t *testing.T) {
	h, tenantID := setupContentModerationHandlerTest(t)
	store := h.contentModerationStore()
	profile, err := contentmoderation.NewProfile(tenantID, "profile-provider-cleanup", contentmoderation.CreateProfileInput{Name: "provider cleanup"}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err = store.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := profile.ID
	if err = store.PatchBindings(context.Background(), tenantID, false, []contentmoderation.BindingOperation{{
		ChannelType: contentmoderation.ChannelTypeProviderKey,
		ChannelID:   "11111111-1111-4111-8111-111111111111",
		ProfileID:   &profileID,
	}}); err != nil {
		t.Fatalf("PatchBindings: %v", err)
	}

	c, rec := moderationContext(tenantID, http.MethodDelete, "/v0/management/gemini-api-key?index=0", "")
	h.ProviderKeys().DeleteGeminiKey(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE provider status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, _, err = store.ResolveProfile(context.Background(), tenantID, "", "11111111-1111-4111-8111-111111111111", ""); !errors.Is(err, contentmoderation.ErrNotFound) {
		t.Fatalf("ResolveProfile error=%v, want ErrNotFound", err)
	}
}
