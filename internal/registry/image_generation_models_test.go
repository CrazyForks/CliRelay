package registry

import "testing"

func TestIsImageGenerationModel(t *testing.T) {
	imageModels := []string{
		"gpt-image-2",
		"GPT-Image-2",
		"gpt-image-3",
		"grok-imagine-image",
		"grok-imagine-image-pro",
		"grok-2-image-1212",
	}
	for _, modelID := range imageModels {
		if !IsImageGenerationModel(modelID) {
			t.Errorf("IsImageGenerationModel(%q) = false, want true", modelID)
		}
	}

	textModels := []string{"", "grok-4.5", "grok-build-0.1", "gpt-5.5", "grok-imagine-video"}
	for _, modelID := range textModels {
		if IsImageGenerationModel(modelID) {
			t.Errorf("IsImageGenerationModel(%q) = true, want false", modelID)
		}
	}
}

// TestUnknownImageModelVersionsStillClassify is why the prefix list exists: a new
// release in a known family must be billed per call on day one, not per token.
func TestUnknownImageModelVersionsStillClassify(t *testing.T) {
	if !IsImageGenerationModel("gpt-image-9-preview") {
		t.Error("an unreleased gpt-image version should still classify as image generation")
	}
	if price, _, ok := ImageGenerationModelDefaults("gpt-image-9-preview"); ok || price != 0 {
		t.Error("a prefix-only match must not invent a price")
	}
}

// TestVideoModelsAreNotImageModels guards the boundary while video support is being
// built out: grok-imagine-video shares the family prefix but is not an image model.
func TestVideoModelsAreNotImageModels(t *testing.T) {
	if IsImageGenerationModel("grok-imagine-video") {
		t.Error("grok-imagine-video must not classify as an image generation model")
	}
}

func TestImageGenerationModelDefaults(t *testing.T) {
	price, description, ok := ImageGenerationModelDefaults("gpt-image-2")
	if !ok {
		t.Fatal("gpt-image-2 should have defaults")
	}
	if price != 0.04 {
		t.Errorf("price = %v, want 0.04 to preserve existing billing", price)
	}
	if description == "" {
		t.Error("description should be set")
	}

	// Grok Imagine is billed against the subscription, so no per-call price is
	// published. Inventing one would show up as real spend in usage reporting.
	if price, _, ok := ImageGenerationModelDefaults("grok-imagine-image"); !ok || price != 0 {
		t.Errorf("grok-imagine-image price = %v, want 0", price)
	}

	if _, _, ok := ImageGenerationModelDefaults("grok-4.5"); ok {
		t.Error("a text model should have no image defaults")
	}
}

// TestImageInputModalityStaysTextOnly pins a deliberate decision: edit support is
// not the same as an image input modality, and widening it would reclassify
// existing models as image-to-image in the catalog.
func TestImageInputModalityStaysTextOnly(t *testing.T) {
	for _, modelID := range []string{"gpt-image-2", "grok-imagine-image", "grok-2-image-1212"} {
		got := ImageGenerationInputModalities(modelID)
		if len(got) != 1 || got[0] != "text" {
			t.Errorf("ImageGenerationInputModalities(%q) = %v, want [text]", modelID, got)
		}
	}
}

func TestSupportsImageEditing(t *testing.T) {
	for _, modelID := range []string{"gpt-image-2", "grok-imagine-image", "grok-imagine-image-pro"} {
		if !SupportsImageEditing(modelID) {
			t.Errorf("SupportsImageEditing(%q) = false, want true", modelID)
		}
	}
	// grok-2-image-1212 has no edit endpoint; offering the control would only
	// produce upstream 404s.
	if SupportsImageEditing("grok-2-image-1212") {
		t.Error("grok-2-image-1212 has no edit endpoint and must not advertise one")
	}
}

func TestImageGenerationOutputModalitiesIsNotShared(t *testing.T) {
	first := ImageGenerationOutputModalities()
	first[0] = "mutated"
	if second := ImageGenerationOutputModalities(); second[0] != "image" {
		t.Error("callers must not be able to mutate the shared modality set")
	}
}

// TestGrokImageModelsAreRegistered ties the classifier to the static catalog: a
// model that classifies as image generation but is absent from the catalog would
// never be selectable.
func TestGrokImageModelsAreRegistered(t *testing.T) {
	registered := make(map[string]struct{})
	for _, model := range GetXAIModels() {
		registered[model.ID] = struct{}{}
	}
	for _, modelID := range []string{"grok-imagine-image", "grok-imagine-image-pro", "grok-2-image-1212"} {
		if _, ok := registered[modelID]; !ok {
			t.Errorf("%s is classified as an image model but is not in the xAI catalog", modelID)
		}
	}
}
