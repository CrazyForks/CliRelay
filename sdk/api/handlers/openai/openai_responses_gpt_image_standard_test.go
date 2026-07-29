package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type responsesCaptureExecutor struct {
	alt          string
	streamAlt    string
	sourceFormat string
	streamCalls  int
	calls        int
}

func (e *responsesCaptureExecutor) Identifier() string { return "codex" }

func (e *responsesCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	e.alt = opts.Alt
	e.sourceFormat = opts.SourceFormat.String()
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *responsesCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.streamCalls++
	e.streamAlt = opts.Alt
	e.sourceFormat = opts.SourceFormat.String()
	ch := make(chan coreexecutor.StreamChunk, 1)
	ch <- coreexecutor.StreamChunk{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *responsesCaptureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *responsesCaptureExecutor) CountTokens(_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesCaptureExecutor) HttpRequest(_ context.Context, _ *coreauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIResponsesRejectsGPTImage2BeforeNonStreamingExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &responsesCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-gpt-image-2-responses", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: openAIImageModelID}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-image-2","input":"draw a fox"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", executor.calls)
	}
	if body := resp.Body.String(); !strings.Contains(body, `"type":"invalid_request_error"`) || !strings.Contains(body, "/v1/images/generations") {
		t.Fatalf("body = %s, want local image-endpoint guidance", body)
	}
}

func TestOpenAIResponsesRejectsGPTImage2BeforeStreamingExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &responsesCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)

	auth := &coreauth.Auth{ID: "auth-gpt-image-2-responses-stream", Provider: executor.Identifier(), Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: openAIImageModelID}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-image-2","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Responses(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if executor.streamCalls != 0 {
		t.Fatalf("stream calls = %d, want 0", executor.streamCalls)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"type":"invalid_request_error"`) || !strings.Contains(body, "/v1/images/generations") {
		t.Fatalf("body = %s, want local image-endpoint guidance", body)
	}
}
