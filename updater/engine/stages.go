package engine

import (
	"clirelay.local/updater/protocol"
)

// pullWeight makes the image pull dominate the progress bar, because it dominates
// the wall clock. Giving every step an equal share is what produced the old
// behaviour where the bar reached 20% in a second and then sat there for minutes.
const pullWeight = 6

// stageTracker converts a plan into a weighted progress timeline and keeps the
// per-stage states the panel renders.
//
// Percentages are monotonic by construction: a segment's contribution is bounded by
// its weight, and completed segments are never recomputed. A progress bar that moves
// backwards reads as a fault even when the update is healthy.
type stageTracker struct {
	segments []*segment
	total    int
}

type segment struct {
	id     string
	weight int
	state  string
	// filled is the fraction of this segment's weight already earned, 0..1.
	filled float64
}

func newStageTracker(plan *protocol.Plan) *stageTracker {
	tracker := &stageTracker{}
	if plan.Prepare != nil {
		tracker.add(protocol.StagePreparing, 1)
	}
	if len(plan.Pull) > 0 {
		tracker.add(protocol.StagePulling, pullWeight)
	}
	for _, stage := range plan.Stages {
		weight := stage.Weight
		if weight <= 0 {
			weight = 1
		}
		tracker.add(stage.ID, weight)
	}
	return tracker
}

func (t *stageTracker) add(id string, weight int) {
	t.segments = append(t.segments, &segment{id: id, weight: weight, state: protocol.StageStatePending})
	t.total += weight
}

func (t *stageTracker) find(id string) *segment {
	for _, candidate := range t.segments {
		if candidate.id == id {
			return candidate
		}
	}
	return nil
}

// percent computes the overall completion from every segment's filled fraction.
func (t *stageTracker) percent() float64 {
	if t.total <= 0 {
		return 0
	}
	earned := 0.0
	for _, candidate := range t.segments {
		earned += float64(candidate.weight) * candidate.filled
	}
	value := earned * 100 / float64(t.total)
	if value > 100 {
		return 100
	}
	if value < 0 {
		return 0
	}
	return value
}

// percentWithin reports the overall percentage when the named segment is `inner`
// percent complete, clamped so a segment can never exceed its own weight.
func (t *stageTracker) percentWithin(id string, inner float64) float64 {
	target := t.find(id)
	if target == nil {
		return t.percent()
	}
	if inner < 0 {
		inner = 0
	}
	if inner > 100 {
		inner = 100
	}
	if fraction := inner / 100; fraction > target.filled {
		target.filled = fraction
	}
	return t.percent()
}

func (t *stageTracker) states() []StageState {
	states := make([]StageState, 0, len(t.segments))
	for _, candidate := range t.segments {
		states = append(states, StageState{ID: candidate.id, State: candidate.state})
	}
	return states
}

// begin marks a segment active and reports the transition.
func (t *stageTracker) begin(reporter Reporter, id string, messageCode string, message string) {
	if target := t.find(id); target != nil {
		target.state = protocol.StageStateActive
	}
	reporter.Progress(ProgressUpdate{
		Stage:       id,
		MessageCode: messageCode,
		Message:     message,
		Percent:     t.percent(),
		Stages:      t.states(),
	})
}

// beginStage marks a plan stage active, deriving its message code from its id so a
// newly added stage needs no updater change to be described.
func (t *stageTracker) beginStage(reporter Reporter, id string) {
	t.begin(reporter, id, "stage_"+id, "running "+id)
}

func (t *stageTracker) complete(reporter Reporter, id string) {
	if target := t.find(id); target != nil {
		target.state = protocol.StageStateDone
		target.filled = 1
	}
	reporter.Progress(ProgressUpdate{
		Stage:       id,
		MessageCode: "stage_" + id + "_done",
		Message:     "finished " + id,
		Percent:     t.percent(),
		Stages:      t.states(),
	})
}

func (t *stageTracker) completeStage(reporter Reporter, id string) {
	t.complete(reporter, id)
}

func (t *stageTracker) skip(reporter Reporter, id string) {
	if target := t.find(id); target != nil {
		target.state = protocol.StageStateSkipped
		// A skipped segment still earns its weight; otherwise the bar could never
		// reach 100% on a deployment that legitimately has nothing to do here.
		target.filled = 1
	}
	reporter.Progress(ProgressUpdate{
		Stage:       id,
		MessageCode: "stage_" + id + "_skipped",
		Message:     "skipped " + id,
		Percent:     t.percent(),
		Stages:      t.states(),
	})
}

func (t *stageTracker) fail(reporter Reporter, id string) {
	if target := t.find(id); target != nil {
		target.state = protocol.StageStateFailed
	}
}

// finish marks everything complete so the bar lands exactly on 100%.
func (t *stageTracker) finish(reporter Reporter) {
	for _, candidate := range t.segments {
		if candidate.state == protocol.StageStateActive || candidate.state == protocol.StageStatePending {
			candidate.state = protocol.StageStateDone
		}
		candidate.filled = 1
	}
	reporter.Progress(ProgressUpdate{
		Stage:       protocol.StageVerifying,
		MessageCode: "service_healthy",
		Message:     "the updated services are running and healthy",
		Percent:     100,
		Stages:      t.states(),
	})
}
