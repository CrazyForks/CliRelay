package updateflow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"clirelay.local/updater/protocol"
)

func TestTriggerUpdateForwardsVersionAndReleaseMetadata(t *testing.T) {
	updater := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/update" {
			t.Fatalf("path = %q, want /v1/update", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want Bearer test-token", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		// Decoded as a flat map on purpose: this is exactly what an updater sidecar
		// that predates plans sees, so the assertion doubles as a guarantee that the
		// legacy fields survive alongside the nested plan.
		var flat map[string]any
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&flat); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for key, want := range map[string]string{
			"current_version":      "main-old",
			"version":              "main-new",
			"commit_url":           "https://example.com/backend",
			"ui_commit_url":        "https://example.com/ui",
			"release_name":         "CliRelay v0.5.0",
			"release_tag":          "v0.5.0",
			"release_notes":        "latest changes",
			"release_published_at": "2026-07-10T07:30:00Z",
		} {
			if got, _ := flat[key].(string); got != want {
				t.Fatalf("payload[%q] = %q, want %q", key, got, want)
			}
		}

		var request protocol.UpdateRequest
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&request); err != nil {
			t.Fatalf("decode plan request: %v", err)
		}
		if request.Plan == nil {
			t.Fatal("no update plan was sent")
		}
		if _, err := request.Plan.Validate(); err != nil {
			t.Fatalf("the plan the application sends must be valid: %v", err)
		}
		if request.Plan.Stages[len(request.Plan.Stages)-1].Services[0] != "clirelay" {
			t.Fatalf("plan does not target the configured service: %+v", request.Plan.Stages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"accepted","service":"clirelay","run_id":17}`))
	}))
	t.Cleanup(updater.Close)
	t.Setenv("CLIRELAY_UPDATER_URL", updater.URL)
	t.Setenv("CLIRELAY_UPDATER_TOKEN", "test-token")
	t.Setenv("CLIRELAY_TARGET_SERVICE", "clirelay")

	result, err := New(nil, Dependencies{}).TriggerUpdate(context.Background(), &CheckResponse{
		DockerImage:        "ghcr.io/kittors/clirelay",
		DockerTag:          "main",
		CurrentVersion:     "main-old",
		LatestVersion:      "main-new",
		LatestCommitURL:    "https://example.com/backend",
		LatestUICommitURL:  "https://example.com/ui",
		ReleaseName:        "CliRelay v0.5.0",
		ReleaseTag:         "v0.5.0",
		ReleaseNotes:       "latest changes",
		ReleasePublishedAt: "2026-07-10T07:30:00Z",
	})
	if err != nil {
		t.Fatalf("TriggerUpdate failed: %v", err)
	}
	if result.RunID != 17 || result.Status != "accepted" {
		t.Fatalf("result = %+v", result)
	}
}
