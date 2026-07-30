package protocol

import "strings"

// Run statuses. These strings are persisted to disk and compared by the web panel;
// they are part of the frozen contract.
const (
	StatusIdle      = "idle"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Well-known stage identifiers. A plan may introduce new ones; the panel falls back
// to a generic label for stages it does not recognize, so adding a stage does not
// require a coordinated frontend release.
const (
	StageIdle       = "idle"
	StagePreparing  = "preparing"
	StagePulling    = "pulling"
	StageRecreating = "recreating"
	StageVerifying  = "verifying"
	StageFinalizing = "finalizing"
	StageCompleted  = "completed"
	StageFailed     = "failed"
)

// Stage lifecycle states used by the panel's stage timeline.
const (
	StageStatePending = "pending"
	StageStateActive  = "active"
	StageStateDone    = "done"
	StageStateFailed  = "failed"
	StageStateSkipped = "skipped"
)

// LogEntry is a single line of streamed command output.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

// StageStatus is one entry in the run's stage timeline.
type StageStatus struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// Status is the payload of GET /v1/status and of every server-sent update event.
//
// Numeric progress fields deliberately omit `omitempty`. With it, a legitimate 0
// (an update that has just started, or a pull that has fetched no bytes yet)
// disappears from the JSON and is indistinguishable from "the updater does not
// report progress" — which made the panel fall back to an indeterminate bar
// rendered at full width at the exact moment the update began.
type Status struct {
	RunID   uint64 `json:"run_id,omitempty"`
	EventID uint64 `json:"event_id,omitempty"`

	Status      string `json:"status"`
	Stage       string `json:"stage"`
	MessageCode string `json:"message_code,omitempty"`
	Message     string `json:"message,omitempty"`

	ProgressPercent float64 `json:"progress_percent"`
	ProgressCurrent int     `json:"progress_current"`
	ProgressTotal   int     `json:"progress_total"`
	ProgressUnit    string  `json:"progress_unit,omitempty"`

	// Byte counters for the image pull, which dominates wall-clock time. Reported
	// only while pulling; zero elsewhere.
	ProgressBytes      int64 `json:"progress_bytes"`
	ProgressTotalBytes int64 `json:"progress_total_bytes"`

	// Stages mirrors the plan's stage list so the panel can render a timeline
	// without knowing the plan.
	Stages []StageStatus `json:"stages,omitempty"`

	Service string `json:"service,omitempty"`

	CurrentVersion   string `json:"current_version,omitempty"`
	CurrentCommit    string `json:"current_commit,omitempty"`
	CurrentUIVersion string `json:"current_ui_version,omitempty"`
	CurrentUICommit  string `json:"current_ui_commit,omitempty"`

	TargetImage       string `json:"target_image,omitempty"`
	TargetTag         string `json:"target_tag,omitempty"`
	TargetVersion     string `json:"target_version,omitempty"`
	TargetCommit      string `json:"target_commit,omitempty"`
	TargetCommitURL   string `json:"target_commit_url,omitempty"`
	TargetUIVersion   string `json:"target_ui_version,omitempty"`
	TargetUICommit    string `json:"target_ui_commit,omitempty"`
	TargetUICommitURL string `json:"target_ui_commit_url,omitempty"`
	TargetChannel     string `json:"target_channel,omitempty"`

	ReleaseName        string `json:"release_name,omitempty"`
	ReleaseTag         string `json:"release_tag,omitempty"`
	ReleaseNotes       string `json:"release_notes,omitempty"`
	ReleaseURL         string `json:"release_url,omitempty"`
	ReleasePublishedAt string `json:"release_published_at,omitempty"`

	StartedAt  string `json:"started_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	Logs []LogEntry `json:"logs,omitempty"`
}

// Clone returns a deep copy so a snapshot handed to a subscriber cannot be mutated
// by the run that produced it.
func (s Status) Clone() Status {
	clone := s
	if len(s.Logs) > 0 {
		clone.Logs = append([]LogEntry(nil), s.Logs...)
	}
	if len(s.Stages) > 0 {
		clone.Stages = append([]StageStatus(nil), s.Stages...)
	}
	return clone
}

// IsTerminal reports whether the run reached a final state.
func (s Status) IsTerminal() bool {
	switch strings.ToLower(strings.TrimSpace(s.Status)) {
	case StatusCompleted, StatusFailed:
		return true
	default:
		return false
	}
}

// IsRunning reports whether an update is currently in flight.
func (s Status) IsRunning() bool {
	return strings.EqualFold(strings.TrimSpace(s.Status), StatusRunning)
}
