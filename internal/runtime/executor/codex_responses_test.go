package executor

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeCodexResponsesRequestStripsUnsupportedTokenLimitFields(t *testing.T) {
	input := []byte(`{"model":"gpt-5.4","max_output_tokens":1024,"max_completion_tokens":2048,"max_tokens":4096,"stream":true}`)
	got := sanitizeCodexResponsesRequest(input)

	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("%s should be stripped for codex upstream; payload=%s", field, got)
		}
	}
	if gotModel := gjson.GetBytes(got, "model").String(); gotModel != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4; payload=%s", gotModel, got)
	}
	if !gjson.GetBytes(got, "stream").Bool() {
		t.Fatalf("stream should be preserved; payload=%s", got)
	}
}

func TestSanitizeCodexResponsesRequestStripsStoredHistoryItemIDsWhenStoreFalse(t *testing.T) {
	input := []byte(`{
		"model":"gpt-5.4",
		"store":false,
		"input":[
			{"type":"message","id":"msg_previous","role":"assistant","content":[{"type":"output_text","text":"done"}]},
			{"type":"function_call","id":"fc_previous","call_id":"call_previous","name":"lookup","arguments":"{\"q\":\"x\"}","status":"completed"},
			{"type":"reasoning","id":"rs_previous","encrypted_content":"encrypted","summary":[]}
		]
	}`)

	got := sanitizeCodexResponsesRequest(input)
	for _, id := range []string{"msg_previous", "fc_previous", "rs_previous"} {
		if bytes.Contains(got, []byte(`"`+id+`"`)) {
			t.Fatalf("stored item id %q should be stripped; payload=%s", id, got)
		}
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "done" {
		t.Fatalf("message content = %q, want done; payload=%s", text, got)
	}
	if callID := gjson.GetBytes(got, "input.1.call_id").String(); callID != "call_previous" {
		t.Fatalf("function call_id = %q, want call_previous; payload=%s", callID, got)
	}
	if arguments := gjson.GetBytes(got, "input.1.arguments").String(); arguments != `{"q":"x"}` {
		t.Fatalf("function arguments = %q; payload=%s", arguments, got)
	}
	if encrypted := gjson.GetBytes(got, "input.2.encrypted_content").String(); encrypted != "encrypted" {
		t.Fatalf("reasoning encrypted_content = %q; payload=%s", encrypted, got)
	}
}

func TestSanitizeCodexResponsesRequestKeepsStoredHistoryItemIDsWhenStoreTrue(t *testing.T) {
	input := []byte(`{"store":true,"input":[{"type":"message","id":"msg_persisted","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)

	got := sanitizeCodexResponsesRequest(input)
	if id := gjson.GetBytes(got, "input.0.id").String(); id != "msg_persisted" {
		t.Fatalf("stored item id = %q, want msg_persisted; payload=%s", id, got)
	}
}

func TestCodexResponsesFailedStatusErrExtractsTopLevelError(t *testing.T) {
	err := codexResponsesFailedStatusErr([]byte(`{"type":"error","code":"internal_server_error","message":"upstream exploded"}`))

	if err.StatusCode() != http.StatusBadGateway {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusBadGateway)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("Error() = %q, want upstream message", err.Error())
	}
	if strings.Contains(err.Error(), "responses request failed") {
		t.Fatalf("Error() = %q, should not use fallback message", err.Error())
	}
}

func TestCodexResponsesFailedStatusErrMapsRateLimit(t *testing.T) {
	err := codexResponsesFailedStatusErr([]byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}}`))

	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want %d", err.StatusCode(), http.StatusTooManyRequests)
	}
	if !strings.Contains(err.Error(), "Rate limit reached") {
		t.Fatalf("Error() = %q, want upstream message", err.Error())
	}
}

func TestSanitizeCodexResponsesRequestMovesImageToolSizeToUserPromptHint(t *testing.T) {
	input := []byte(`{
		"model":"gpt-5.4-mini",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"stay concise"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"draw a poster"}]}
		],
		"tools":[{"type":"image_generation","model":"gpt-image-2","size":"4096x2304","quality":"high"}],
		"tool_choice":{"type":"image_generation"}
	}`)

	got := sanitizeCodexResponsesRequest(input)

	if gjson.GetBytes(got, "tools.0.size").Exists() {
		t.Fatalf("tools.0.size should be stripped for codex upstream; payload=%s", got)
	}
	if quality := gjson.GetBytes(got, "tools.0.quality").String(); quality != "high" {
		t.Fatalf("tools.0.quality = %q, want high; payload=%s", quality, got)
	}
	if developerText := gjson.GetBytes(got, "input.0.content.0.text").String(); strings.Contains(developerText, codexResponsesImageSizeHintPrefix) {
		t.Fatalf("developer message should not receive size hint; text=%q payload=%s", developerText, got)
	}
	userText := gjson.GetBytes(got, "input.1.content.0.text").String()
	if !strings.Contains(userText, "draw a poster") || !strings.Contains(userText, "Preferred image size: 4096x2304.") {
		t.Fatalf("user message should keep prompt and receive size hint; text=%q payload=%s", userText, got)
	}
}
