package engine

import (
	"testing"

	"clirelay.local/updater/protocol"
)

type recordingReporter struct {
	updates []ProgressUpdate
	logs    []string
}

func (r *recordingReporter) Stage(string, string) {}

func (r *recordingReporter) Progress(update ProgressUpdate) {
	r.updates = append(r.updates, update)
}

func (r *recordingReporter) Log(_ string, message string) {
	r.logs = append(r.logs, message)
}

func (r *recordingReporter) percents() []float64 {
	values := make([]float64, 0, len(r.updates))
	for _, update := range r.updates {
		values = append(values, update.Percent)
	}
	return values
}

func samplePlan() *protocol.Plan {
	plan := &protocol.Plan{
		PlanVersion: 1,
		Image:       "ghcr.io/kittors/clirelay",
		Tag:         "dev",
		Prepare:     &protocol.PrepareStep{Command: []string{"clirelay-init-env"}},
		Pull:        []string{"cli-proxy-api"},
		Stages: []protocol.Stage{
			{ID: "dependencies", Services: []string{"postgres", "redis"}, Optional: true, WaitHealthy: true},
			{ID: "application", Services: []string{"cli-proxy-api"}, WaitHealthy: true, Weight: 2},
		},
	}
	normalized, err := plan.Validate()
	if err != nil {
		panic(err)
	}
	return normalized
}

// TestStageTrackerProgressIsMonotonic is the property that matters most to the user
// experience: a bar that moves backwards reads as a fault even on a healthy update.
func TestStageTrackerProgressIsMonotonic(t *testing.T) {
	tracker := newStageTracker(samplePlan())
	reporter := &recordingReporter{}

	tracker.begin(reporter, protocol.StagePreparing, "preparing", "preparing")
	tracker.complete(reporter, protocol.StagePreparing)
	tracker.begin(reporter, protocol.StagePulling, "pulling", "pulling")
	for _, inner := range []float64{5, 40, 40, 95, 100} {
		tracker.percentWithin(protocol.StagePulling, inner)
		reporter.Progress(ProgressUpdate{Percent: tracker.percent()})
	}
	tracker.complete(reporter, protocol.StagePulling)
	tracker.beginStage(reporter, "dependencies")
	tracker.completeStage(reporter, "dependencies")
	tracker.beginStage(reporter, "application")
	tracker.completeStage(reporter, "application")
	tracker.finish(reporter)

	previous := -1.0
	for index, percent := range reporter.percents() {
		if percent < previous {
			t.Fatalf("progress went backwards at update %d: %v -> %v", index, previous, percent)
		}
		previous = percent
	}
	if previous != 100 {
		t.Errorf("final percent = %v, want 100", previous)
	}
}

// TestStageTrackerPullDominatesTheBar pins the weighting: the pull is minutes long
// and must own most of the bar, not one step of five.
func TestStageTrackerPullDominatesTheBar(t *testing.T) {
	tracker := newStageTracker(samplePlan())
	reporter := &recordingReporter{}

	tracker.complete(reporter, protocol.StagePreparing)
	atPullStart := tracker.percent()
	tracker.percentWithin(protocol.StagePulling, 100)
	atPullEnd := tracker.percent()

	if share := atPullEnd - atPullStart; share < 50 {
		t.Errorf("the pull only accounts for %v%% of the bar; it should dominate", share)
	}
}

// TestStageTrackerSkippedStageStillReachesFull covers deployments whose compose file
// predates an optional stage: they must still be able to reach 100%.
func TestStageTrackerSkippedStageStillReachesFull(t *testing.T) {
	tracker := newStageTracker(samplePlan())
	reporter := &recordingReporter{}

	tracker.complete(reporter, protocol.StagePreparing)
	tracker.complete(reporter, protocol.StagePulling)
	tracker.skip(reporter, "dependencies")
	tracker.completeStage(reporter, "application")

	if percent := tracker.percent(); percent != 100 {
		t.Errorf("percent = %v, want 100 after skipping an optional stage", percent)
	}
	for _, state := range tracker.states() {
		if state.ID == "dependencies" && state.State != protocol.StageStateSkipped {
			t.Errorf("dependencies state = %q, want %q", state.State, protocol.StageStateSkipped)
		}
	}
}

func TestStageTrackerPercentWithinNeverRegresses(t *testing.T) {
	tracker := newStageTracker(samplePlan())
	tracker.percentWithin(protocol.StagePulling, 80)
	high := tracker.percent()
	tracker.percentWithin(protocol.StagePulling, 10)

	if got := tracker.percent(); got != high {
		t.Errorf("a lower inner percent regressed the bar: %v -> %v", high, got)
	}
}

func TestPresentServicesFiltersToWhatTheDeploymentDefines(t *testing.T) {
	available := map[string]struct{}{"cli-proxy-api": {}, "redis": {}}

	got := presentServices([]string{"postgres", "redis"}, available)
	if len(got) != 1 || got[0] != "redis" {
		t.Errorf("presentServices = %v, want [redis]", got)
	}

	// A nil availability set means compose could not be queried; attempting every
	// service is safer than silently skipping the update.
	if got := presentServices([]string{"postgres"}, nil); len(got) != 1 {
		t.Errorf("presentServices with unknown availability = %v, want the input unchanged", got)
	}
}
