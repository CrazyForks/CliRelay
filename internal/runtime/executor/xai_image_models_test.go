package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

func modelIDs(models []*sdkmodelcatalog.ModelInfo) map[string]struct{} {
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}

const grokImagineModel = "grok-imagine-image"

// TestDiscoveredModelsCarryImageModels is the regression this fixes. The Imagine
// models are not discoverable — the OAuth subscription's /models endpoint is the
// CLI chat proxy, which lists chat models only. Registering them in the static
// catalog made them visible in the panel while no credential actually served them,
// so a request failed with "auth_not_found: no auth available" before reaching xAI.
func TestDiscoveredModelsCarryImageModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.5","object":"model"}]}`))
	}))
	t.Cleanup(upstream.Close)

	models := FetchXAIModels(
		context.Background(),
		oauthAuth(upstream.URL+"/v1"),
		&config.Config{},
	)

	ids := modelIDs(models)
	if _, ok := ids["grok-4.5"]; !ok {
		t.Error("the live chat model list was lost")
	}
	for _, modelID := range []string{grokImagineModel, "grok-imagine-image-pro", "grok-2-image-1212"} {
		if _, ok := ids[modelID]; !ok {
			t.Errorf("%s is not routable; the credential cannot serve it", modelID)
		}
	}
}

// TestImageModelsRemainRoutableWhenDiscoveryFails covers a credential whose /models
// call fails. The Imagine models are not discovered in the first place, so a failed
// discovery must not make them disappear.
func TestImageModelsRemainRoutableWhenDiscoveryFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	models := FetchXAIModels(context.Background(), oauthAuth(upstream.URL+"/v1"), &config.Config{})
	if _, ok := modelIDs(models)[grokImagineModel]; !ok {
		t.Error("image models must stay routable when model discovery fails")
	}
}

func TestImageModelsRemainRoutableWithoutACredential(t *testing.T) {
	models := FetchXAIModels(context.Background(), nil, &config.Config{})
	if _, ok := modelIDs(models)[grokImagineModel]; !ok {
		t.Error("image models should still be listed without a credential")
	}
}

// TestUpstreamWinsWhenItAdvertisesAnImageModel keeps upstream authoritative about
// anything it does report, so a live definition is never shadowed by the static one.
func TestUpstreamWinsWhenItAdvertisesAnImageModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-imagine-image","object":"model","context_length":4242}]}`))
	}))
	t.Cleanup(upstream.Close)

	models := FetchXAIModels(context.Background(), oauthAuth(upstream.URL+"/v1"), &config.Config{})

	count := 0
	var found *sdkmodelcatalog.ModelInfo
	for _, model := range models {
		if model != nil && model.ID == grokImagineModel {
			count++
			found = model
		}
	}
	if count != 1 {
		t.Fatalf("grok-imagine-image appears %d times, want exactly 1", count)
	}
	if found.ContextLength != 4242 {
		t.Error("the static definition shadowed what upstream reported")
	}
}

func TestImageModelInfosCoverEveryRegisteredImageModel(t *testing.T) {
	if len(xaiImageModelInfos()) != 3 {
		t.Errorf("image model count = %d, want the three registered Grok image models", len(xaiImageModelInfos()))
	}
	for _, model := range xaiImageModelInfos() {
		if model.Type != "xai" {
			t.Errorf("%s type = %q, want xai so provider routing resolves", model.ID, model.Type)
		}
	}
}
