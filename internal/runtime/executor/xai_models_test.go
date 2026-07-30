package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

func resetXAIModelsCacheForTest() {
	xaiModelsCache.mu.Lock()
	xaiModelsCache.models = nil
	xaiModelsCache.mu.Unlock()
}

func TestFetchXAIModelsParsesLiveCatalogAndAliases(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != xaiModelsPath {
			t.Fatalf("request path = %q, want %q", r.URL.Path, xaiModelsPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xai-token" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "grok-build-0.1",
					"aliases": ["grok-code-fast-1", "grok-code-fast"],
					"object": "model",
					"created": 1776297600,
					"owned_by": "xai",
					"context_length": 256000,
					"max_completion_tokens": 128000
				},
				{
					"id": "grok-4.5",
					"aliases": ["grok-build-latest"],
					"object": "model",
					"created": 1782691200,
					"owned_by": "xai",
					"context_length": 500000
				}
			]
		}`))
	}))
	defer srv.Close()

	models := FetchXAIModels(context.Background(), &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":  "xai-token",
			"base_url": srv.URL,
		},
	}, nil)

	byID := make(map[string]*sdkmodelcatalog.ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			byID[model.ID] = model
		}
	}
	if byID["grok-build-0.1"] == nil {
		t.Fatal("missing upstream grok-build-0.1 model")
	}
	if got := byID["grok-build-0.1"].ContextLength; got != 256000 {
		t.Fatalf("grok-build-0.1 context length = %d, want 256000", got)
	}
	if got := byID["grok-code-fast-1"].UpstreamModelID; got != "grok-build-0.1" {
		t.Fatalf("grok-code-fast-1 upstream = %q, want grok-build-0.1", got)
	}
	if got := byID["grok-build-latest"].UpstreamModelID; got != "grok-4.5" {
		t.Fatalf("grok-build-latest upstream = %q, want grok-4.5", got)
	}
	if got := byID["grok-build"].UpstreamModelID; got != "grok-build-0.1" {
		t.Fatalf("grok-build compatibility upstream = %q, want grok-build-0.1", got)
	}
}

func TestFetchXAIModelsFallsBackToCachedCatalog(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)

	if ok := storeXAIModels([]*sdkmodelcatalog.ModelInfo{{ID: "cached-xai-model", OwnedBy: "xai", Type: "xai"}}); !ok {
		t.Fatal("expected cache seed to store")
	}

	models := FetchXAIModels(context.Background(), &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":  "xai-token",
			"base_url": "http://127.0.0.1:1",
		},
	}, nil)
	// The cached chat model must survive. It is no longer the only entry: the Grok
	// Imagine models are not discoverable through /models at all, so they are merged
	// into every credential's set and remain routable when discovery fails.
	found := false
	for _, model := range models {
		if model != nil && model.ID == "cached-xai-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fallback models = %+v, want the cached model retained", models)
	}
}

type captureRoundTripper struct {
	req *http.Request
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.req = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"grok-4.5","object":"model","owned_by":"xai"}]}`)),
		Request:    req,
	}, nil
}

func TestFetchXAIModelsUsesCLIChatProxyWhenUsingAPIFalse(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)

	rt := &captureRoundTripper{}
	ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, rt)

	models := FetchXAIModels(ctx, &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"api_key":   "oauth-token",
			"base_url":  xaiauth.DefaultAPIBaseURL,
			"using_api": "false",
		},
	}, nil)

	if rt.req == nil {
		t.Fatal("expected models request to be issued")
	}
	wantURL := strings.TrimRight(xaiauth.CLIChatProxyBaseURL, "/") + xaiModelsPath
	if got := rt.req.URL.String(); got != wantURL {
		t.Fatalf("models URL = %q, want %q", got, wantURL)
	}
	if got := rt.req.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("Authorization = %q, want Bearer oauth-token", got)
	}
	if got := rt.req.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
		t.Fatalf("%s = %q, want %q", xaiTokenAuthHeader, got, xaiTokenAuthValue)
	}
	if got := rt.req.Header.Get("X-Grok-Client-Version"); got != config.DefaultXAIFingerprintClientVersion {
		t.Fatalf("X-Grok-Client-Version = %q, want %q", got, config.DefaultXAIFingerprintClientVersion)
	}
	if len(models) == 0 || models[0].ID != "grok-4.5" {
		t.Fatalf("models = %+v, want grok-4.5", models)
	}
}

func TestFetchXAIModelsKeepsOfficialAPIWhenUsingAPITrue(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)

	rt := &captureRoundTripper{}
	ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, rt)

	_ = FetchXAIModels(ctx, &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"api_key":   "oauth-token",
			"base_url":  xaiauth.DefaultAPIBaseURL,
			"using_api": "true",
		},
	}, nil)

	if rt.req == nil {
		t.Fatal("expected models request to be issued")
	}
	wantURL := strings.TrimRight(xaiauth.DefaultAPIBaseURL, "/") + xaiModelsPath
	if got := rt.req.URL.String(); got != wantURL {
		t.Fatalf("models URL = %q, want %q", got, wantURL)
	}
	if got := rt.req.Header.Get(xaiTokenAuthHeader); got != "" {
		t.Fatalf("%s = %q, want empty for official API", xaiTokenAuthHeader, got)
	}
	if got := rt.req.Header.Get("X-Grok-Client-Version"); got != "" {
		t.Fatalf("X-Grok-Client-Version = %q, want empty for official API", got)
	}
}

func TestFetchXAIModelsKeepsCustomGatewayBaseURL(t *testing.T) {
	resetXAIModelsCacheForTest()
	t.Cleanup(resetXAIModelsCacheForTest)

	rt := &captureRoundTripper{}
	ctx := context.WithValue(context.Background(), util.ContextKeyRoundTripper, rt)
	customBase := "https://gateway.example.com/v1"

	_ = FetchXAIModels(ctx, &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"api_key":   "oauth-token",
			"base_url":  customBase,
			"using_api": "false",
		},
	}, nil)

	if rt.req == nil {
		t.Fatal("expected models request to be issued")
	}
	wantURL := strings.TrimRight(customBase, "/") + xaiModelsPath
	if got := rt.req.URL.String(); got != wantURL {
		t.Fatalf("models URL = %q, want %q", got, wantURL)
	}
	if got := rt.req.Header.Get(xaiTokenAuthHeader); got != "" {
		t.Fatalf("%s = %q, want empty for custom gateway", xaiTokenAuthHeader, got)
	}
}
