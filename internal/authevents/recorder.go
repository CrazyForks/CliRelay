package authevents

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// queueCapacity bounds the in-flight buffer. Sized to absorb a burst of a
	// few seconds at attack rates without becoming a memory sink of its own.
	queueCapacity = 4096
	// batchSize is the maximum rows per multi-row insert.
	batchSize = 128
	// flushInterval bounds how long a low-volume event waits before it is
	// visible in the panel.
	flushInterval = 2 * time.Second
	// dropWarnInterval deduplicates the overflow warning, which by definition
	// fires when the process is already under load.
	dropWarnInterval = time.Minute
)

// Recorder persists events off the request path.
//
// Writes are asynchronous and lossy by design: this is security telemetry, and
// a telemetry backlog must never be able to slow down or block a login. When the
// queue overflows the events are dropped and counted, never buffered without
// bound and never written synchronously as a fallback.
type Recorder struct {
	db      *sql.DB
	queue   chan Event
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	dropped atomic.Int64

	warnMu   sync.Mutex
	lastWarn time.Time
}

// NewRecorder builds a recorder. A nil db yields a recorder whose Record is a
// no-op, so callers never need to check.
func NewRecorder(db *sql.DB) *Recorder {
	return &Recorder{
		db:    db,
		queue: make(chan Event, queueCapacity),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Available reports whether events can be persisted.
func (r *Recorder) Available() bool { return r != nil && r.db != nil }

// Record queues an event. It never blocks and never returns an error: no
// authentication path should be able to fail because logging failed.
func (r *Recorder) Record(event Event) {
	if !r.Available() {
		return
	}
	select {
	case r.queue <- event.sanitized():
	default:
		r.dropped.Add(1)
		r.warnDropped()
	}
}

func (r *Recorder) warnDropped() {
	now := time.Now()
	r.warnMu.Lock()
	if !r.lastWarn.IsZero() && now.Sub(r.lastWarn) < dropWarnInterval {
		r.warnMu.Unlock()
		return
	}
	r.lastWarn = now
	r.warnMu.Unlock()
	log.Warnf("auth-events: queue full, dropped %d events so far; attempt history is incomplete for this period", r.dropped.Load())
}

// Dropped reports how many events were discarded. Surfacing this matters: a
// report built from a truncated event log would otherwise look authoritative.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// Start launches the writer goroutine.
func (r *Recorder) Start(ctx context.Context) {
	if !r.Available() {
		return
	}
	go r.loop(ctx)
}

func (r *Recorder) loop(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	pending := make([]Event, 0, batchSize)

	flush := func() {
		if len(pending) == 0 {
			return
		}
		r.write(ctx, pending)
		pending = pending[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-r.stop:
			// Drain what is already queued so a graceful shutdown does not lose
			// the events describing whatever prompted the restart.
			for {
				select {
				case event := <-r.queue:
					pending = append(pending, event)
					if len(pending) >= batchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		case event := <-r.queue:
			pending = append(pending, event)
			if len(pending) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Recorder) write(ctx context.Context, events []Event) {
	if len(events) == 0 {
		return
	}
	// A dedicated timeout keeps a stalled database from pinning the writer
	// goroutine and letting the queue back up into drops.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	var builder strings.Builder
	builder.WriteString(`INSERT INTO auth_attempt_events
		(occurred_at,ip,ip_prefix,trusted,scope,surface,username,outcome,reason,user_agent,request_path,request_id,tenant_id) VALUES `)
	args := make([]any, 0, len(events)*13)
	for i := range events {
		if i > 0 {
			builder.WriteString(",")
		}
		builder.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?)")
		event := events[i]
		var tenantID any
		if strings.TrimSpace(event.TenantID) != "" {
			tenantID = event.TenantID
		}
		args = append(args, event.OccurredAt.UTC(), event.IP, event.IPPrefix, event.Trusted,
			event.Scope, event.Surface, event.Username, event.Outcome, event.Reason,
			event.UserAgent, event.RequestPath, event.RequestID, tenantID)
	}
	if _, err := r.db.ExecContext(writeCtx, builder.String(), args...); err != nil {
		log.WithError(err).Warnf("auth-events: failed to persist %d events", len(events))
	}
}

// Close stops the writer and waits for the final flush.
func (r *Recorder) Close() {
	if !r.Available() {
		return
	}
	r.once.Do(func() {
		close(r.stop)
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			log.Warn("auth-events: writer did not drain within 5s")
		}
	})
}

var defaultRecorder atomic.Pointer[Recorder]

// SetDefault publishes the process-wide recorder.
func SetDefault(r *Recorder) { defaultRecorder.Store(r) }

// Default returns the process-wide recorder, or nil when none is configured.
func Default() *Recorder { return defaultRecorder.Load() }

// Record sends an event to the process-wide recorder when one exists.
func Record(event Event) {
	if recorder := Default(); recorder != nil {
		recorder.Record(event)
	}
}
