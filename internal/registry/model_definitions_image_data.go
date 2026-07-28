package registry

// Image-generation model definitions.
//
// These live apart from the general static catalog for two reasons: that file is
// already over the structure gate's line budget and may only shrink, and image
// models carry billing and capability rules that the classifier in
// image_generation_models.go reads. Keeping the definitions next to the rules that
// interpret them means adding a model is one file, not two.

// getOpenAIImageModelDefinitions returns OpenAI image-generation models.
func getOpenAIImageModelDefinitions() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:                  "gpt-image-2",
			Object:              "model",
			OwnedBy:             "openai",
			Type:                "openai",
			Version:             "gpt-image-2",
			DisplayName:         "GPT Image 2",
			Description:         "Text-to-image generation model.",
			SupportedParameters: []string{"prompt", "size", "n", "response_format"},
		},
	}
}

// getXAIImageModelDefinitions returns Grok Imagine image-generation models.
//
// Media requests reach the official API host even for subscription credentials,
// because the CLI gateway rejects the payload sizes these carry; see
// xaiMediaBaseURL in the runtime executor.
func getXAIImageModelDefinitions() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:          "grok-imagine-image",
			Object:      "model",
			OwnedBy:     "xai",
			Type:        "xai",
			Version:     "grok-imagine-image",
			DisplayName: "Grok Imagine",
			Name:        "grok-imagine-image",
			Description: "Grok Imagine text-to-image generation with reference image editing.",
			// Media requests go to the official API host even for subscription
			// credentials; see xaiMediaBaseURL in the runtime executor.
			SupportedParameters: []string{"prompt", "n", "response_format", "image", "mask"},
		},
		{
			ID:                  "grok-imagine-image-pro",
			Object:              "model",
			OwnedBy:             "xai",
			Type:                "xai",
			Version:             "grok-imagine-image-pro",
			DisplayName:         "Grok Imagine Pro",
			Name:                "grok-imagine-image-pro",
			Description:         "Higher fidelity Grok Imagine image generation.",
			SupportedParameters: []string{"prompt", "n", "response_format", "image", "mask"},
		},
		{
			ID:          "grok-2-image-1212",
			Object:      "model",
			OwnedBy:     "xai",
			Type:        "xai",
			Version:     "grok-2-image-1212",
			DisplayName: "Grok 2 Image",
			Name:        "grok-2-image-1212",
			Description: "Grok 2 text-to-image generation.",
			// No image input: this model has no edit endpoint, so advertising one
			// would only produce upstream 404s.
			SupportedParameters: []string{"prompt", "n", "response_format"},
		},
	}
}
