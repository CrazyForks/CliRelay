package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func oauthAuth(baseURL string) *cliproxyauth.Auth {
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Metadata: map[string]any{
			"access_token": "token-123",
			"auth_kind":    "oauth",
		},
	}
	if baseURL != "" {
		auth.Metadata["base_url"] = baseURL
	}
	return auth
}

func apiKeyAuth(baseURL string) *cliproxyauth.Auth {
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"api_key": "sk-test"},
	}
	if baseURL != "" {
		auth.Attributes["base_url"] = baseURL
	}
	return auth
}

// TestMediaLeavesTheCLIGatewayForOAuth is the load-bearing rule. Chat over an OAuth
// subscription resolves to the CLI gateway, but that gateway rejects the base64
// payloads media requests carry, so media has to go to the official API host.
func TestMediaLeavesTheCLIGatewayForOAuth(t *testing.T) {
	auth := oauthAuth("")

	if chat := xaiChatBaseURL(auth); !xaiIsBaseURL(chat, xaiauth.CLIChatProxyBaseURL) {
		t.Fatalf("precondition failed: OAuth chat should use the CLI gateway, got %q", chat)
	}

	endpoint, err := xaiMediaEndpointFor(auth, xaiImageGenerationAlt)
	if err != nil {
		t.Fatalf("resolve media endpoint: %v", err)
	}
	want := xaiauth.DefaultAPIBaseURL + "/images/generations"
	if endpoint != want {
		t.Errorf("media endpoint = %q, want %q", endpoint, want)
	}
}

// TestMediaKeepsAnOperatorSelectedEndpoint covers the deliberate override: a pinned
// regional host or relay is usually pinned because the default is unreachable for
// that deployment, so media must not silently route around it.
func TestMediaKeepsAnOperatorSelectedEndpoint(t *testing.T) {
	const relay = "https://relay.example.com/v1"

	for name, auth := range map[string]*cliproxyauth.Auth{
		"oauth":   oauthAuth(relay),
		"api key": apiKeyAuth(relay),
	} {
		endpoint, err := xaiMediaEndpointFor(auth, xaiImageGenerationAlt)
		if err != nil {
			t.Fatalf("%s: resolve media endpoint: %v", name, err)
		}
		if endpoint != relay+"/images/generations" {
			t.Errorf("%s: media endpoint = %q, want the pinned relay", name, endpoint)
		}
	}
}

func TestMediaEndpointPerAlt(t *testing.T) {
	auth := apiKeyAuth(xaiauth.DefaultAPIBaseURL)

	cases := map[string]string{
		xaiImageGenerationAlt: xaiauth.DefaultAPIBaseURL + "/images/generations",
		xaiImageEditsAlt:      xaiauth.DefaultAPIBaseURL + "/images/edits",
	}
	for alt, want := range cases {
		got, err := xaiMediaEndpointFor(auth, alt)
		if err != nil {
			t.Fatalf("alt %q: %v", alt, err)
		}
		if got != want {
			t.Errorf("alt %q endpoint = %q, want %q", alt, got, want)
		}
	}

	if _, err := xaiMediaEndpointFor(auth, "responses"); err == nil {
		t.Error("a non-media alt should be rejected")
	}
}

func TestExecuteImageGenerationPassesThroughUpstreamPayload(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"AAAA","revised_prompt":"a cat"}]}`))
	}))
	t.Cleanup(upstream.Close)

	executor := NewXAIExecutor(&config.Config{})
	auth := oauthAuth(upstream.URL + "/v1")
	request := cliproxyexecutor.Request{
		Model:   "grok-imagine-image",
		Payload: []byte(`{"model":"grok-imagine-image","prompt":"a cat","n":1}`),
	}

	resp, err := executor.Execute(context.Background(), auth, request, cliproxyexecutor.Options{Alt: xaiImageGenerationAlt})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotPath != "/v1/images/generations" {
		t.Errorf("upstream path = %q, want /v1/images/generations", gotPath)
	}
	if gotAuth != "Bearer token-123" {
		t.Errorf("Authorization = %q, want the OAuth access token", gotAuth)
	}
	if !strings.Contains(gotBody, `"prompt":"a cat"`) {
		t.Errorf("request body was not forwarded verbatim: %q", gotBody)
	}

	// The upstream is OpenAI-compatible, so the response must reach the caller
	// unmodified rather than through a lossy re-encode.
	var decoded struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Payload, &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(decoded.Data) != 1 || decoded.Data[0].B64JSON != "AAAA" {
		t.Fatalf("response payload lost data: %s", resp.Payload)
	}
	if decoded.Data[0].RevisedPrompt != "a cat" {
		t.Error("a field the proxy does not model was dropped; media must pass through")
	}
}

// TestExecuteImageGenerationDoesNotSendCLIGatewayHeaders guards against leaking the
// Grok CLI client headers to the public API host, which only the CLI gateway wants.
func TestExecuteImageGenerationDoesNotSendCLIGatewayHeaders(t *testing.T) {
	var headers http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(upstream.Close)

	executor := NewXAIExecutor(&config.Config{})
	_, err := executor.Execute(
		context.Background(),
		oauthAuth(upstream.URL+"/v1"),
		cliproxyexecutor.Request{Model: "grok-imagine-image", Payload: []byte(`{"prompt":"x"}`)},
		cliproxyexecutor.Options{Alt: xaiImageGenerationAlt},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := headers.Get(xaiTokenAuthHeader); got != "" {
		t.Errorf("%s was sent to the media host: %q", xaiTokenAuthHeader, got)
	}
	if got := headers.Get("X-Grok-Client-Version"); got != "" {
		t.Errorf("X-Grok-Client-Version was sent to the media host: %q", got)
	}
}

func TestExecuteImageGenerationSurfacesUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"balance exhausted"}}`))
	}))
	t.Cleanup(upstream.Close)

	executor := NewXAIExecutor(&config.Config{})
	_, err := executor.Execute(
		context.Background(),
		oauthAuth(upstream.URL+"/v1"),
		cliproxyexecutor.Request{Model: "grok-imagine-image", Payload: []byte(`{"prompt":"x"}`)},
		cliproxyexecutor.Options{Alt: xaiImageGenerationAlt},
	)
	if err == nil {
		t.Fatal("an upstream 402 must surface as an error")
	}
	// A media call spends the same subscription balance as chat, so it has to land
	// in the same quota-cooldown handling rather than looking like a generic failure.
	coder, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error does not carry a status code: %T", err)
	}
	if coder.StatusCode() != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", coder.StatusCode())
	}
}

func TestExecuteImageGenerationRejectsMissingCredential(t *testing.T) {
	executor := NewXAIExecutor(&config.Config{})
	_, err := executor.Execute(
		context.Background(),
		&cliproxyauth.Auth{Provider: "xai"},
		cliproxyexecutor.Request{Model: "grok-imagine-image", Payload: []byte(`{"prompt":"x"}`)},
		cliproxyexecutor.Options{Alt: xaiImageGenerationAlt},
	)
	if err == nil {
		t.Fatal("a credential without an access token must be rejected")
	}
}

func TestExecuteImageGenerationStreamEmitsASingleChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"AAAA"}]}`))
	}))
	t.Cleanup(upstream.Close)

	executor := NewXAIExecutor(&config.Config{})
	result, err := executor.ExecuteStream(
		context.Background(),
		oauthAuth(upstream.URL+"/v1"),
		cliproxyexecutor.Request{Model: "grok-imagine-image", Payload: []byte(`{"prompt":"x"}`)},
		cliproxyexecutor.Options{Alt: xaiImageGenerationAlt},
	)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	// The endpoint has no streaming form; callers going through the shared images
	// handler should still get a well-formed terminal chunk instead of an error.
	count := 0
	for chunk := range result.Chunks {
		count++
		if !strings.Contains(string(chunk.Payload), "b64_json") {
			t.Errorf("chunk payload = %q", chunk.Payload)
		}
	}
	if count != 1 {
		t.Errorf("chunk count = %d, want 1", count)
	}
}
