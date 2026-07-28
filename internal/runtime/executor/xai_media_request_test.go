package executor

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestShapeDropsArgumentsXAIRejects is the fix for the failure an operator hits on
// the very first attempt: the console sends size and quality by default, and xAI
// rejects the whole request with 400 "Argument not supported: size".
func TestShapeDropsArgumentsXAIRejects(t *testing.T) {
	body := []byte(`{
		"model":"grok-imagine-image-quality",
		"prompt":"a cat",
		"n":1,
		"response_format":"b64_json",
		"size":"1024x1024",
		"quality":"medium",
		"style":"vivid",
		"background":"transparent",
		"output_format":"png"
	}`)

	shaped, dropped := shapeXAIImageRequest(body)

	var decoded map[string]any
	if err := json.Unmarshal(shaped, &decoded); err != nil {
		t.Fatalf("shaped body is not valid json: %v", err)
	}
	for _, field := range []string{"size", "quality", "style", "background", "output_format"} {
		if _, present := decoded[field]; present {
			t.Errorf("%q survived and will be rejected upstream", field)
		}
		if !slices.Contains(dropped, field) {
			t.Errorf("%q was removed but not reported to the caller", field)
		}
	}

	// Everything the endpoint documents has to survive untouched.
	for field, want := range map[string]any{
		"model": "grok-imagine-image-quality", "prompt": "a cat", "response_format": "b64_json",
	} {
		if decoded[field] != want {
			t.Errorf("%q = %v, want %v", field, decoded[field], want)
		}
	}
	if decoded["n"] != float64(1) {
		t.Errorf("n = %v, want 1", decoded["n"])
	}
}

func TestShapeLeavesASupportedRequestUntouched(t *testing.T) {
	body := []byte(`{"model":"grok-imagine-image","prompt":"x","n":2,"response_format":"url"}`)

	shaped, dropped := shapeXAIImageRequest(body)
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want nothing", dropped)
	}
	if string(shaped) != string(body) {
		t.Errorf("body was rewritten unnecessarily: %s", shaped)
	}
}

// TestShapeKeepsEditInputs guards the edits endpoint: stripping the reference image
// would turn an edit into a plain generation without saying so.
func TestShapeKeepsEditInputs(t *testing.T) {
	body := []byte(`{"model":"grok-imagine-image","prompt":"x","image":"data:image/png;base64,AAA","mask":"data:image/png;base64,BBB"}`)

	shaped, dropped := shapeXAIImageRequest(body)
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v, want nothing", dropped)
	}
	var decoded map[string]any
	if err := json.Unmarshal(shaped, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["image"] == nil || decoded["mask"] == nil {
		t.Error("reference inputs were stripped from an edit request")
	}
}

func TestShapeIgnoresNonObjectBodies(t *testing.T) {
	for _, body := range []string{`[]`, `"text"`, ``} {
		shaped, dropped := shapeXAIImageRequest([]byte(body))
		if string(shaped) != body || len(dropped) != 0 {
			t.Errorf("body %q was modified: %q %v", body, shaped, dropped)
		}
	}
}
