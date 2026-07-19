package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestUnifiedModelsHandlerScopesBusinessTenantToPortalPlazaAndAllowedModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		businessTenant = "cccccccc-dddd-eeee-ffff-000000000001"
		businessAuthID = "models-business-tenant-codex-auth"
		visibleModelA  = "gpt-business-visible-a"
		visibleModelB  = "gpt-business-visible-b"
		extraModel     = "gpt-business-static-extra"
		discoveryOnly  = "gpt-business-discovery-only"
	)

	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(businessAuthID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(businessAuthID) })
	modelRegistry.RegisterClient(businessAuthID, "codex", []*registry.ModelInfo{
		{ID: visibleModelA, Object: "model", OwnedBy: "openai"},
		{ID: visibleModelB, Object: "model", OwnedBy: "openai"},
		{ID: extraModel, Object: "model", OwnedBy: "openai"},
	})
	// Management plaza replaces the Codex static registry catalog with this
	// tenant-scoped live discovery set. discoveryOnly is intentionally absent
	// from the runtime registry, while extraModel is intentionally absent here.
	managementauthfiles.StoreDiscoveryCacheForTest(businessTenant, "codex", []*registry.ModelInfo{
		{ID: visibleModelA, Object: "model", OwnedBy: "openai"},
		{ID: visibleModelB, Object: "model", OwnedBy: "openai"},
		{ID: discoveryOnly, Object: "model", OwnedBy: "openai"},
	})

	authManager := coreauth.NewManager(nil, nil, nil)
	authManager.SetConfigForTenant(businessTenant, &config.Config{})
	if _, err := authManager.Register(context.Background(), &coreauth.Auth{
		ID: businessAuthID, TenantID: businessTenant, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register business auth: %v", err)
	}

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager)
	server := &Server{handlers: base, cfg: cfg}
	openaiHandler := openai.NewOpenAIAPIHandler(base)
	claudeHandler := claude.NewClaudeCodeAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set("tenantID", businessTenant)
		if c.Query("restricted") == "1" {
			// Include extraModel to prove allowed-models cannot re-introduce an ID
			// removed by the plaza truth source.
			c.Set("accessMetadata", map[string]string{"allowed-models": visibleModelA + "," + extraModel})
		}
		server.unifiedModelsHandler(openaiHandler, claudeHandler)(c)
	})

	responseIDs := func(target, userAgent string) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		}
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body=%s", target, rec.Code, rec.Body.String())
		}
		var response struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal GET %s response: %v", target, err)
		}
		return modelIDs(response.Data)
	}

	ids := responseIDs("/v1/models", "")
	if !sameStringSet(ids, []string{visibleModelA, visibleModelB}) {
		t.Fatalf("business tenant OpenAI models = %#v, want configured∩root-path registry models", ids)
	}

	ids = responseIDs("/v1/models", "claude-cli/1.0")
	if !sameStringSet(ids, []string{visibleModelA, visibleModelB}) {
		t.Fatalf("business tenant Claude models = %#v, want configured∩root-path registry models", ids)
	}

	ids = responseIDs("/v1/models?restricted=1", "")
	if !sameStringSet(ids, []string{visibleModelA}) {
		t.Fatalf("restricted business tenant models = %#v, want only %q", ids, visibleModelA)
	}
}

func TestUnifiedModelsHandlerKeepsSystemTenantRegistryBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		systemAuthID = "models-system-tenant-codex-auth"
		visibleModel = "gpt-system-live"
		staticOnly   = "gpt-system-static-only"
	)

	managementauthfiles.ResetDiscoveryCacheForTest()
	t.Cleanup(managementauthfiles.ResetDiscoveryCacheForTest)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(systemAuthID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(systemAuthID) })
	modelRegistry.RegisterClient(systemAuthID, "codex", []*registry.ModelInfo{
		{ID: visibleModel, Object: "model", OwnedBy: "openai"},
		{ID: staticOnly, Object: "model", OwnedBy: "openai"},
	})
	managementauthfiles.StoreDiscoveryCacheForTest(identity.SystemTenantID, "codex", []*registry.ModelInfo{
		{ID: visibleModel, Object: "model", OwnedBy: "openai"},
	})

	authManager := coreauth.NewManager(nil, nil, nil)
	authManager.SetConfigForTenant(identity.SystemTenantID, &config.Config{})
	if _, err := authManager.Register(context.Background(), &coreauth.Auth{
		ID: systemAuthID, TenantID: identity.SystemTenantID, Provider: "codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register system auth: %v", err)
	}

	cfg := &config.Config{}
	base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager)
	server := &Server{handlers: base, cfg: cfg}
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		c.Set("tenantID", identity.SystemTenantID)
		server.unifiedModelsHandler(
			openai.NewOpenAIAPIHandler(base),
			claude.NewClaudeCodeAPIHandler(base),
		)(c)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	ids := modelIDs(response.Data)
	if !containsString(ids, visibleModel) || !containsString(ids, staticOnly) {
		t.Fatalf("system tenant models = %#v, want both registry models", ids)
	}
}

func TestFilterCodexModelsForCcSwitchRouteReturnsRequestModels(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "deepseek-chat", "object": "model", "owned_by": "deepseek"},
		{"id": "kimi-k2", "object": "model", "owned_by": "moonshot"},
		{"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
	}
	route := &internalrouting.PathRouteContext{
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ClientType:   "codex",
			DefaultModel: "deepseek-v4-flash",
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{RequestModel: "deepseek-v4-flash", TargetModel: "deepseek-chat"},
				{RequestModel: "deepseek-v4-pro", TargetModel: "deepseek-chat"},
			},
		},
	}

	filtered := filterModelsForCcSwitchRoute(models, route)
	got := modelIDs(filtered)
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro"}
	if !sameStrings(got, want) {
		t.Fatalf("model ids = %#v, want %#v", got, want)
	}
	if filtered[0]["owned_by"] != "deepseek" {
		t.Fatalf("owned_by = %v, want deepseek", filtered[0]["owned_by"])
	}
}

func TestCcSwitchRequestModelAllowedForTarget(t *testing.T) {
	route := &internalrouting.PathRouteContext{
		CcSwitch: &internalrouting.CcSwitchRouteContext{
			ModelMappings: []internalrouting.CcSwitchModelMapping{
				{RequestModel: "deepseek-v4-flash", TargetModel: "deepseek-chat"},
			},
		},
	}

	if !ccSwitchRequestModelAllowedForTarget("deepseek-chat", route, map[string]struct{}{"deepseek-v4-flash": {}}) {
		t.Fatal("request model alias was not allowed for target")
	}
	if ccSwitchRequestModelAllowedForTarget("kimi-k2", route, map[string]struct{}{"deepseek-v4-flash": {}}) {
		t.Fatal("unmapped target was allowed")
	}
}

func TestEnrichOpenAIModelsWithCatalogLeavesUnknownModels(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "unknown-model-xyz", "object": "model"},
	}
	enrichOpenAIModelsWithCatalog("", models)
	if _, hasPricing := models[0]["pricing"]; hasPricing {
		t.Fatal("unexpected pricing for unknown model")
	}
	if _, hasDesc := models[0]["description"]; hasDesc {
		t.Fatal("unexpected description for unknown model")
	}
}

func modelIDs(models []map[string]interface{}) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id, ok := model["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, value := range a {
		if !containsString(b, value) {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
