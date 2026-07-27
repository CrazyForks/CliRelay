package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOllamaCloudExecutorRoutesChatToOpenAICompatibleV1(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","created_at":"2026-07-07T00:00:00Z","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL + "/v1"}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"gpt-oss:120b","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %q, want /api/chat", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "keep_alive").String(); got != ollamaCloudNativeKeepAlive {
		t.Fatalf("keep_alive = %q, want %q; body=%s", got, ollamaCloudNativeKeepAlive, gotBody)
	}
	if gotText := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotText != "ok" {
		t.Fatalf("response text = %q, want ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOllamaCloudExecutorStripsImagesForNonVisionModelsAcrossIngressFormats(t *testing.T) {
	tests := []struct {
		name     string
		source   sdktranslator.Format
		payload  string
		wantPath string
	}{
		{
			name:     "openai",
			source:   sdktranslator.FormatOpenAI,
			payload:  `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`,
			wantPath: "/api/chat",
		},
		{
			name:     "claude",
			source:   sdktranslator.FormatClaude,
			payload:  `{"model":"deepseek-v4-flash","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`,
			wantPath: "/v1/messages",
		},
		{
			name:     "gemini",
			source:   sdktranslator.FormatGemini,
			payload:  `{"model":"deepseek-v4-flash","contents":[{"role":"user","parts":[{"text":"describe"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`,
			wantPath: "/v1/chat/completions",
		},
		{
			name:     "openai tool history",
			source:   sdktranslator.FormatOpenAI,
			payload:  `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"capture the screen"},{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"capture","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"screenshot"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]},{"role":"user","content":"continue with text"}]}`,
			wantPath: "/api/chat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/show":
					_, _ = w.Write([]byte(`{"capabilities":["completion","tools"]}`))
					return
				case "/api/chat":
					_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
				case "/v1/messages":
					_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
				case "/v1/chat/completions":
					_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
				default:
					http.NotFound(w, r)
					return
				}
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
			}))
			defer server.Close()

			exec := NewOllamaCloudExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
			_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "deepseek-v4-flash",
				Payload: []byte(tt.payload),
			}, cliproxyexecutor.Options{SourceFormat: tt.source})
			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, tt.wantPath)
			}
			if strings.Contains(string(gotBody), "aGVsbG8=") || strings.Contains(string(gotBody), `"images"`) || strings.Contains(string(gotBody), `"image_url"`) || strings.Contains(string(gotBody), `"type":"image"`) {
				t.Fatalf("non-vision upstream received raw image content: %s", gotBody)
			}
			if !strings.Contains(string(gotBody), "Image omitted") {
				t.Fatalf("upstream body lacks actionable image placeholder: %s", gotBody)
			}
		})
	}
}

func TestOllamaCloudExecutorTextFollowUpAfterImageUsesCachedNonVisionCapability(t *testing.T) {
	var showCalls int
	var chatBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			showCalls++
			_, _ = w.Write([]byte(`{"capabilities":["completion","tools"]}`))
		case "/api/chat":
			body, _ := io.ReadAll(r.Body)
			chatBodies = append(chatBodies, body)
			_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	requests := []string{
		`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`,
		`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]},{"role":"assistant","content":"ok"},{"role":"user","content":"now answer a text-only follow-up"}]}`,
	}
	for _, payload := range requests {
		if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "deepseek-v4-flash",
			Payload: []byte(payload),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); err != nil {
			t.Fatalf("Execute returned error: %v", err)
		}
	}

	if showCalls != 1 {
		t.Fatalf("/api/show calls = %d, want 1 cached capability lookup", showCalls)
	}
	if len(chatBodies) != 2 {
		t.Fatalf("/api/chat calls = %d, want 2", len(chatBodies))
	}
	for i, body := range chatBodies {
		if strings.Contains(string(body), `"images"`) || strings.Contains(string(body), "aGVsbG8=") {
			t.Fatalf("chat body %d retained image input: %s", i, body)
		}
	}
}

func TestOllamaCloudExecutorPreservesImagesForVisionModel(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			_, _ = w.Write([]byte(`{"capabilities":["completion","tools","vision"]}`))
		case "/api/chat":
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"model":"qwen3.5:397b","message":{"role":"assistant","content":"vision ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3.5:397b",
		Payload: []byte(`{"model":"qwen3.5:397b","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.images.0").String(); got != "aGVsbG8=" {
		t.Fatalf("vision model image = %q, want base64 payload; body=%s", got, gotBody)
	}
}

func TestOllamaCloudExecutorUsesVerifiedVisionFallback(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			body, _ := io.ReadAll(r.Body)
			if gjson.GetBytes(body, "model").String() == "qwen3.5:397b" {
				_, _ = w.Write([]byte(`{"capabilities":["completion","vision"]}`))
			} else {
				_, _ = w.Write([]byte(`{"capabilities":["completion","tools"]}`))
			}
		case "/api/chat":
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"model":"qwen3.5:397b","message":{"role":"assistant","content":"vision ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":               "test-key",
		"base_url":              server.URL,
		"vision_fallback_model": "qwen3.5:397b",
	}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "qwen3.5:397b" {
		t.Fatalf("upstream model = %q, want verified vision fallback; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.images.0").String(); got != "aGVsbG8=" {
		t.Fatalf("fallback image = %q, want preserved image; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(resp.Payload, "model").String(); got != "deepseek-v4-flash" {
		t.Fatalf("response model = %q, want original model; payload=%s", got, resp.Payload)
	}
}

func TestOllamaCloudExecutorStreamStripsImagesForNonVisionModel(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			_, _ = w.Write([]byte(`{"capabilities":["completion"]}`))
		case "/api/chat":
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: []byte(`{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	if strings.Contains(string(gotBody), "aGVsbG8=") || gjson.GetBytes(gotBody, "messages.0.images").Exists() {
		t.Fatalf("stream upstream received image input: %s", gotBody)
	}
}

func TestOllamaCloudNativeCacheKeyScopesExplicitPromptKey(t *testing.T) {
	source := []byte(`{"model":"glm-5.2","prompt_cache_key":"shared-session","input":"hi"}`)
	authA := &cliproxyauth.Auth{ID: "ollama-cloud:one"}
	authB := &cliproxyauth.Auth{ID: "ollama-cloud:two"}

	keyA := ollamaNativeCacheKey(authA, "glm-5.2", source, nil, cliproxyexecutor.Options{})
	keyB := ollamaNativeCacheKey(authB, "glm-5.2", source, nil, cliproxyexecutor.Options{})

	if keyA == "" || keyB == "" {
		t.Fatalf("cache keys must be present: %q / %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("cache key not isolated by auth: %q", keyA)
	}
	if strings.Contains(keyA, "shared-session") || strings.Contains(keyB, "shared-session") {
		t.Fatalf("cache key leaked raw prompt key: %q / %q", keyA, keyB)
	}
}

func TestOllamaCloudExecutorRoutesResponsesToNativeChat(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","created_at":"2026-07-07T00:00:00Z","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"client-prefix/gpt-oss:120b","input":"hi"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %q, want /api/chat", gotPath)
	}
	if gotModel := gjson.GetBytes(gotBody, "model").String(); gotModel != "gpt-oss:120b" {
		t.Fatalf("upstream model = %q, want gpt-oss:120b; body=%s", gotModel, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "prompt_cache_key").Exists() {
		t.Fatalf("native Ollama body must not forward OpenAI prompt_cache_key: %s", gotBody)
	}
	if gotText := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); gotText != "ok" {
		t.Fatalf("response text = %q, want ok; payload=%s", gotText, string(resp.Payload))
	}
}

func TestOllamaCloudNativeChatConvertsToolCallArguments(t *testing.T) {
	body, _ := ollamaNativeChatRequest([]byte(`{
		"model":"deepseek-v4-flash",
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"cat /Users/kittors/.codex/RTK.md\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"}
		]
	}`), true)

	if got := gjson.GetBytes(body, "messages.0.tool_calls.0.function.arguments.cmd").String(); got != "cat /Users/kittors/.codex/RTK.md" {
		t.Fatalf("tool call arguments not converted for Ollama: %s", body)
	}
	if got := gjson.GetBytes(body, "messages.1.tool_name").String(); got != "exec_command" {
		t.Fatalf("tool response name = %q, want exec_command; body=%s", got, body)
	}
}

func TestOllamaCloudExecutorNativeResponsesReportsEstimatedCacheHit(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"glm-5.2","created_at":"2026-07-07T00:00:00Z","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1000,"eval_count":1}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "ollama-cloud:apikey:one", Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	longPrompt := strings.Repeat("cached-prefix ", 500)
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.SessionStickyMetadataKey: "header:x-session-id:session-1",
		},
	}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","input":"` + longPrompt + `"}`),
	}, opts)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","input":"` + longPrompt + ` plus one more turn"}`),
	}, opts)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("server calls = %d, want 2", calls)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.input_tokens_details.cached_tokens").Int(); got <= 0 {
		t.Fatalf("cached_tokens = %d, want positive cache estimate; payload=%s", got, resp.Payload)
	}
}

func TestOllamaCloudExecutorStreamsResponsesFromNativeChat(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","message":{"role":"assistant","content":"ok"},"done":false}` + "\n"))
		_, _ = w.Write([]byte(`{"model":"gpt-oss:120b","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}` + "\n"))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"gpt-oss:120b","input":"hi","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	var chunks strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		chunks.Write(chunk.Payload)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %q, want /api/chat", gotPath)
	}
	if got := chunks.String(); !strings.Contains(got, "response.completed") || !strings.Contains(got, "ok") {
		t.Fatalf("stream payload = %q, want responses stream with ok completion", got)
	}
}

func TestOllamaCloudExecutorClaudeMessagesAddsCacheControl(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"glm-5.2","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","max_tokens":1024,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"again"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.0.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("cache_control = %q, want ephemeral; body=%s", got, gotBody)
	}
}

func TestOllamaCloudExecutorRoutesClaudeMessagesToOfficialEndpoint(t *testing.T) {
	var gotPath, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.Header.Get("Anthropic-Version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"gpt-oss:120b","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	exec := NewOllamaCloudExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-oss:120b",
		Payload: []byte(`{"model":"gpt-oss:120b","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", gotPath)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q, want 2023-06-01", gotVersion)
	}
	if gotText := gjson.GetBytes(resp.Payload, "content.0.text").String(); gotText != "ok" {
		t.Fatalf("response text = %q, want ok; payload=%s", gotText, string(resp.Payload))
	}
}
