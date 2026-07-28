package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"clirelay.local/updater/engine"
	"clirelay.local/updater/protocol"
)

const (
	// maxLogEntries bounds the log tail carried in every snapshot.
	maxLogEntries = 200

	// replayBufferSize bounds the events retained for Last-Event-ID resume. The
	// window only has to cover a reconnect, and a reconnect happens within seconds.
	replayBufferSize = 256

	// subscriberQueueSize is per-connection. Overflow coalesces to the newest event,
	// which is lossless here because every event is a complete snapshot rather than
	// a delta — see publishLocked.
	subscriberQueueSize = 8
)

// Store owns the updater's run state: the current status, the subscribers watching
// it, and the replay window that lets a reconnecting client resume without gaps.
//
// Resume is not a nicety. The application container is recreated in the middle of
// every update, which tears down the panel's event stream at the exact moment the
// most interesting transitions happen. Without replay the panel would reconnect into
// a hole and could only guess at what it missed.
type Store struct {
	mu     sync.Mutex
	status protocol.Status
	runID  uint64

	subscribers map[chan protocol.Status]struct{}

	// replay holds recent events in ascending EventID order.
	replay []protocol.Status

	stateFile string

	// pullSkipped records a compose pull that was skipped, so a run can fail loudly
	// instead of reporting success while still on the old image.
	pullSkipped bool
	pullSkipLog string
}

func NewStore(stateFile string) *Store {
	store := &Store{
		subscribers: make(map[chan protocol.Status]struct{}),
		stateFile:   strings.TrimSpace(stateFile),
		status: protocol.Status{
			Status: protocol.StatusIdle,
			Stage:  protocol.StageIdle,
		},
	}
	store.restore()
	return store
}

// restore reloads the last persisted status.
//
// EventID continuity across process restarts is load-bearing: the updater replaces
// itself at the end of an update, and a client holding a Last-Event-ID from the
// previous process must not be handed lower ids by the new one, or it would filter
// out the very events telling it the update finished.
func (s *Store) restore() {
	if s.stateFile == "" {
		return
	}
	data, err := os.ReadFile(s.stateFile)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		log.Printf("clirelay updater: read state failed: %v", err)
		return
	}
	var status protocol.Status
	if err := json.Unmarshal(data, &status); err != nil {
		log.Printf("clirelay updater: decode state failed: %v", err)
		return
	}
	if strings.TrimSpace(status.Status) == "" || strings.TrimSpace(status.Stage) == "" {
		return
	}

	s.status = status.Clone()
	s.runID = status.RunID

	// A run recorded as still running cannot have survived this process dying, so it
	// is reported as failed rather than left pending forever. The exception is a run
	// that already reached its final stage: that is the expected shape of the
	// updater refreshing itself immediately after a successful update.
	if status.IsRunning() {
		now := nowRFC3339()
		if status.Stage == protocol.StageFinalizing || status.ProgressPercent >= 100 {
			s.status.Status = protocol.StatusCompleted
			s.status.Stage = protocol.StageCompleted
			s.status.MessageCode = "completed"
			s.status.Message = "update completed"
			s.status.ProgressPercent = 100
		} else {
			s.status.Status = protocol.StatusFailed
			s.status.Stage = protocol.StageFailed
			s.status.MessageCode = "updater_restarted"
			s.status.Message = "the updater restarted before the update completed"
		}
		s.status.UpdatedAt = now
		s.status.FinishedAt = now
		s.publishLocked(true)
	}
}

func (s *Store) persistLocked() {
	if s.stateFile == "" {
		return
	}
	data, err := json.MarshalIndent(s.status, "", "  ")
	if err != nil {
		log.Printf("clirelay updater: encode state failed: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.stateFile), 0o755); err != nil {
		log.Printf("clirelay updater: create state directory failed: %v", err)
		return
	}
	// Write-and-rename so a crash mid-write cannot leave a truncated state file that
	// the replacement process would refuse to parse.
	tmp := s.stateFile + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		log.Printf("clirelay updater: write state failed: %v", err)
		return
	}
	if err := os.Rename(tmp, s.stateFile); err != nil {
		_ = os.Remove(tmp)
		log.Printf("clirelay updater: replace state failed: %v", err)
	}
}

// publishLocked stamps a new event id, optionally persists, appends to the replay
// window and fans out to subscribers.
func (s *Store) publishLocked(persist bool) {
	s.status.EventID++
	if persist {
		s.persistLocked()
	}
	snapshot := s.status.Clone()

	s.replay = append(s.replay, snapshot)
	if len(s.replay) > replayBufferSize {
		s.replay = append([]protocol.Status(nil), s.replay[len(s.replay)-replayBufferSize:]...)
	}

	for subscriber := range s.subscribers {
		select {
		case subscriber <- snapshot:
		default:
			// The subscriber is behind. Every event is a full snapshot carrying the
			// cumulative log tail, so discarding the oldest queued event and pushing
			// the newest loses no state — the newest strictly supersedes it.
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- snapshot:
			default:
			}
		}
	}
}

// Subscribe registers a listener. It returns the events to replay immediately
// (everything the caller missed, or the current snapshot when the caller is too far
// behind for the replay window), the live channel, and an unsubscribe function.
func (s *Store) Subscribe(lastEventID uint64) ([]protocol.Status, <-chan protocol.Status, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan protocol.Status, subscriberQueueSize)
	s.subscribers[ch] = struct{}{}

	replay := s.replayFromLocked(lastEventID)

	return replay, ch, func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
	}
}

func (s *Store) replayFromLocked(lastEventID uint64) []protocol.Status {
	current := s.status.Clone()
	if lastEventID == 0 || len(s.replay) == 0 {
		return []protocol.Status{current}
	}
	// Already current: nothing to replay, but the client still needs a snapshot to
	// render from if it reconnected after a process restart.
	if lastEventID >= current.EventID {
		return []protocol.Status{current}
	}
	if oldest := s.replay[0].EventID; lastEventID < oldest-1 {
		// Too far behind for the window; a full snapshot resynchronizes it.
		return []protocol.Status{current}
	}

	missed := make([]protocol.Status, 0, len(s.replay))
	for _, event := range s.replay {
		if event.EventID > lastEventID {
			missed = append(missed, event.Clone())
		}
	}
	if len(missed) == 0 {
		return []protocol.Status{current}
	}
	return missed
}

// Snapshot returns the current status.
func (s *Store) Snapshot() protocol.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status.Clone()
}

// StartRun opens a new run, or reports false when one is already in flight.
func (s *Store) StartRun(service string, request protocol.UpdateRequest) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.IsRunning() {
		return s.runID, false
	}

	s.runID++
	now := nowRFC3339()
	// EventID carries over from the previous run so ids stay monotonic for clients
	// that watched both.
	s.status = protocol.Status{
		RunID:              s.runID,
		EventID:            s.status.EventID,
		Status:             protocol.StatusRunning,
		Stage:              protocol.StagePreparing,
		MessageCode:        "preparing_deployment",
		Message:            "preparing deployment configuration",
		Service:            service,
		CurrentVersion:     strings.TrimSpace(request.CurrentVersion),
		CurrentCommit:      strings.TrimSpace(request.CurrentCommit),
		CurrentUIVersion:   strings.TrimSpace(request.CurrentUIVersion),
		CurrentUICommit:    strings.TrimSpace(request.CurrentUICommit),
		TargetImage:        strings.TrimSpace(request.Image),
		TargetTag:          strings.TrimSpace(request.Tag),
		TargetVersion:      strings.TrimSpace(request.Version),
		TargetCommit:       strings.TrimSpace(request.Commit),
		TargetCommitURL:    strings.TrimSpace(request.CommitURL),
		TargetUIVersion:    strings.TrimSpace(request.UIVersion),
		TargetUICommit:     strings.TrimSpace(request.UICommit),
		TargetUICommitURL:  strings.TrimSpace(request.UICommitURL),
		TargetChannel:      strings.TrimSpace(request.Channel),
		ReleaseName:        strings.TrimSpace(request.ReleaseName),
		ReleaseTag:         strings.TrimSpace(request.ReleaseTag),
		ReleaseNotes:       strings.TrimSpace(request.ReleaseNotes),
		ReleaseURL:         strings.TrimSpace(request.ReleaseURL),
		ReleasePublishedAt: strings.TrimSpace(request.ReleasePublishedAt),
		StartedAt:          now,
		UpdatedAt:          now,
	}
	s.pullSkipped = false
	s.pullSkipLog = ""
	s.publishLocked(true)
	return s.runID, true
}

// Finish closes a run.
func (s *Store) Finish(runID uint64, status string, stage string, messageCode string, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != s.runID {
		return
	}
	now := nowRFC3339()
	s.status.Status = strings.TrimSpace(status)
	s.status.Stage = strings.TrimSpace(stage)
	s.status.MessageCode = strings.TrimSpace(messageCode)
	s.status.Message = strings.TrimSpace(message)
	if status == protocol.StatusCompleted {
		if s.status.ProgressTotal > 0 {
			s.status.ProgressCurrent = s.status.ProgressTotal
		}
		s.status.ProgressPercent = 100
		for index := range s.status.Stages {
			if s.status.Stages[index].State == protocol.StageStateActive ||
				s.status.Stages[index].State == protocol.StageStatePending {
				s.status.Stages[index].State = protocol.StageStateDone
			}
		}
	}
	s.status.UpdatedAt = now
	s.status.FinishedAt = now
	s.publishLocked(true)
}

// Reporter returns an engine.Reporter bound to a run.
func (s *Store) Reporter(runID uint64) engine.Reporter {
	return &storeReporter{store: s, runID: runID}
}

type storeReporter struct {
	store *Store
	runID uint64
}

func (r *storeReporter) Stage(stage string, message string) {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.runID != s.runID {
		return
	}
	s.status.Stage = strings.TrimSpace(stage)
	s.status.MessageCode = strings.TrimSpace(stage)
	s.status.Message = strings.TrimSpace(message)
	s.status.UpdatedAt = nowRFC3339()
	s.publishLocked(true)
}

func (r *storeReporter) Progress(update engine.ProgressUpdate) {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.runID != s.runID {
		return
	}
	s.status.Stage = strings.TrimSpace(update.Stage)
	s.status.MessageCode = strings.TrimSpace(update.MessageCode)
	s.status.Message = strings.TrimSpace(update.Message)
	if update.Percent >= 0 {
		// Clamped monotonic: a late-arriving lower value must not rewind the bar.
		if update.Percent > s.status.ProgressPercent {
			s.status.ProgressPercent = update.Percent
		}
	}
	s.status.ProgressBytes = update.Bytes
	s.status.ProgressTotalBytes = update.TotalBytes
	if len(update.Stages) > 0 {
		stages := make([]protocol.StageStatus, 0, len(update.Stages))
		done := 0
		for _, stage := range update.Stages {
			stages = append(stages, protocol.StageStatus{ID: stage.ID, State: stage.State})
			if stage.State == protocol.StageStateDone || stage.State == protocol.StageStateSkipped {
				done++
			}
		}
		s.status.Stages = stages
		s.status.ProgressCurrent = done
		s.status.ProgressTotal = len(stages)
		s.status.ProgressUnit = "steps"
	}
	s.status.UpdatedAt = nowRFC3339()
	s.publishLocked(true)
}

func (r *storeReporter) Log(stream string, message string) {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.runID != s.runID {
		return
	}
	s.status.UpdatedAt = nowRFC3339()
	s.status.Logs = append(s.status.Logs, protocol.LogEntry{
		Timestamp: s.status.UpdatedAt,
		Stream:    strings.TrimSpace(stream),
		Message:   trimmed,
	})
	if len(s.status.Logs) > maxLogEntries {
		s.status.Logs = append([]protocol.LogEntry(nil), s.status.Logs[len(s.status.Logs)-maxLogEntries:]...)
	}
	// Log lines are not persisted: they arrive in bursts during a pull and rewriting
	// the state file per line would turn a chatty pull into thousands of fsyncs.
	s.publishLocked(false)
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
