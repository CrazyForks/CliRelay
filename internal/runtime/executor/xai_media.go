package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	xaiImageGenerationAlt = "images/generations"
	xaiImageEditsAlt      = "images/edits"

	xaiImagesGenerationsPath = "/images/generations"
	xaiImagesEditsPath       = "/images/edits"
)

// xaiIsMediaAlt reports whether a request targets one of the Grok Imagine media
// endpoints rather than the chat surface.
func xaiIsMediaAlt(alt string) bool {
	switch strings.TrimSpace(alt) {
	case xaiImageGenerationAlt, xaiImageEditsAlt:
		return true
	default:
		return false
	}
}

// xaiMediaBaseURL selects the upstream for Grok Imagine requests.
//
// Chat over an OAuth subscription resolves to the CLI gateway, but that gateway
// enforces a small request-body limit and rejects the base64 payloads media
// requests carry. Media therefore leaves for the official API host whenever chat
// would have used the CLI gateway. An operator who pinned some other endpoint —
// a regional host or a relay — keeps it, because that choice is deliberate and
// usually exists precisely because the default is unreachable for them.
func xaiMediaBaseURL(auth *cliproxyauth.Auth) string {
	baseURL := xaiChatBaseURL(auth)
	if xaiIsBaseURL(baseURL, xaiauth.CLIChatProxyBaseURL) {
		return xaiauth.DefaultAPIBaseURL
	}
	return baseURL
}

// xaiMediaPath maps an alt to its upstream path.
func xaiMediaPath(alt string) string {
	if strings.TrimSpace(alt) == xaiImageEditsAlt {
		return xaiImagesEditsPath
	}
	return xaiImagesGenerationsPath
}

// executeImageGeneration forwards an image request to the Grok Imagine API.
//
// The response passes through untranslated because the upstream shape already
// matches: re-encoding it would drop any field xAI adds that we do not model. The
// request cannot be passed through, because xAI rejects OpenAI-only arguments
// outright; see shapeXAIImageRequest.
func (e *XAIExecutor) executeImageGeneration(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	token, _ := xaiCreds(auth)
	if strings.TrimSpace(token) == "" {
		return resp, statusErr{code: http.StatusUnauthorized, msg: "xai credential has no access token"}
	}

	payload := req.Payload
	if len(bytes.TrimSpace(payload)) == 0 {
		return resp, statusErr{code: http.StatusBadRequest, msg: "image request body is empty"}
	}

	// xAI rejects the entire request when an OpenAI-only argument is present, so
	// the body is filtered to what the Imagine endpoints accept.
	payload, dropped := shapeXAIImageRequest(payload)
	if len(dropped) > 0 {
		logWithRequestID(execCtx.Context).Debugf(
			"xai image request: dropped unsupported arguments %s", strings.Join(dropped, ", "),
		)
	}

	endpoint := strings.TrimSuffix(xaiMediaBaseURL(auth), "/") + xaiMediaPath(opts.Alt)
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return resp, err
	}
	// Media never goes to the CLI gateway, so the Grok CLI client headers that
	// gateway requires are deliberately not applied here.
	applyXAIHeaders(httpReq, e.cfg, auth, token, false)

	recorder := execCtx.Recorder()
	recorder.RecordRequest(endpoint, http.MethodPost, httpReq.Header.Clone(), payload)

	httpResp, err := execCtx.HTTPClient(0).Do(httpReq) //nolint:bodyclose // closed by the defer below.
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close image response body error: %v", errClose)
		}
	}()

	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(body)
		logWithRequestID(execCtx.Context).Debugf(
			"xai image request error, status: %d, message: %s",
			httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), body),
		)
		reporter.publishFailureWithContentBytes(execCtx.Context, req.Payload, string(body))
		// Reuse the chat status mapping so a 402 still lands in the weekly quota
		// cooldown path; a media call exhausts the same subscription balance.
		return resp, newXAIStatusErr(httpResp.StatusCode, body, httpResp.Header)
	}

	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)

	return cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

// executeImageGenerationStream serves the streaming entry point.
//
// The Imagine image endpoints have no streaming form, so a single terminal chunk is
// emitted rather than refusing the request: callers reaching this through the shared
// OpenAI images handler should not have to know which providers stream.
func (e *XAIExecutor) executeImageGenerationStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	resp, err := e.executeImageGeneration(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: resp.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

// xaiMediaEndpointFor exposes endpoint resolution for tests and diagnostics.
func xaiMediaEndpointFor(auth *cliproxyauth.Auth, alt string) (string, error) {
	if !xaiIsMediaAlt(alt) {
		return "", fmt.Errorf("alt %q is not an xai media endpoint", alt)
	}
	return strings.TrimSuffix(xaiMediaBaseURL(auth), "/") + xaiMediaPath(alt), nil
}
