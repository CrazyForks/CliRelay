package contentmoderation

import (
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestExtractLastUserText(t *testing.T) {
	tests := []struct {
		name   string
		format sdktranslator.Format
		body   string
		want   string
	}{
		{"openai chat", sdktranslator.FormatOpenAI, `{"messages":[{"role":"assistant","content":"old"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}]}`, "hello world"},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"input":[{"role":"assistant","content":[{"type":"output_text","text":"old"}]},{"role":"user","content":[{"type":"input_text","text":"response text"}]}]}`, "response text"},
		{"claude", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":[{"type":"text","text":"claude text"}]}]}`, "claude text"},
		{"gemini", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"gemini text"}]}]}`, "gemini text"},
		{"last user before assistant", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"last user"},{"role":"assistant","content":"generated"}]}`, "last user"},
		{"responses last user before output", sdktranslator.FormatOpenAIResponse, `{"input":[{"role":"user","content":[{"type":"input_text","text":"response user"}]},{"role":"assistant","content":[{"type":"output_text","text":"generated"}]}]}`, "response user"},
		{"gemini last user before model", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"gemini user"}]},{"role":"model","parts":[{"text":"generated"}]}]}`, "gemini user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractLastUserText(tt.format, []byte(tt.body)); got != tt.want {
				t.Fatalf("ExtractLastUserText = %q, want %q", got, tt.want)
			}
		})
	}
}
