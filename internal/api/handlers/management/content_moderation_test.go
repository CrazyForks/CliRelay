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
