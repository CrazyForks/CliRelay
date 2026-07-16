package usage

import "testing"

func TestExtractSessionIDFromDetailsRecognizesXSessionID(t *testing.T) {
	detail := `{"client":{"headers":{"X-Session-Id":["zcode-session"],"Conversation-Id":["conversation"]}}}`
	if got := extractSessionIDFromDetails(detail); got != "zcode-session" {
		t.Fatalf("session_id = %q, want zcode-session", got)
	}
}

func TestExtractSessionIDFromDetailsRecognizesGrokSessionHeaders(t *testing.T) {
	t.Run("x-grok-session-id", func(t *testing.T) {
		detail := `{"client":{"headers":{"X-Grok-Session-Id":["019f5a53-5c9c-7222-a74c-3dbab60349d3"],"X-Grok-Conv-Id":["should-not-win"]}}}`
		want := "019f5a53-5c9c-7222-a74c-3dbab60349d3"
		if got := extractSessionIDFromDetails(detail); got != want {
			t.Fatalf("session_id = %q, want %s", got, want)
		}
	})

	t.Run("x-grok-conv-id fallback", func(t *testing.T) {
		detail := `{"client":{"fingerprint_headers":{"X-Grok-Conv-Id":["019f5a53-5c9c-7222-a74c-3dbab60349d3"]}}}`
		want := "019f5a53-5c9c-7222-a74c-3dbab60349d3"
		if got := extractSessionIDFromDetails(detail); got != want {
			t.Fatalf("session_id = %q, want %s", got, want)
		}
	})

	t.Run("generic session still beats grok conv", func(t *testing.T) {
		detail := `{"client":{"headers":{"Session-Id":["generic-session"],"X-Grok-Conv-Id":["grok-conv"]}}}`
		if got := extractSessionIDFromDetails(detail); got != "generic-session" {
			t.Fatalf("session_id = %q, want generic-session", got)
		}
	})
}
