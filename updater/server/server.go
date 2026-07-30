package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clirelay.local/updater/engine"
	"clirelay.local/updater/protocol"
)

const (
	// eventHeartbeat keeps intermediaries from closing an idle stream. It is well
	// under the 60s idle timeout common to nginx and cloud load balancers.
	eventHeartbeat = 10 * time.Second

	updateCommandTimeout = 30 * time.Minute
)

// Config describes the deployment this updater manages. None of it says anything
// about what the application is or does.
type Config struct {
	Token          string
	ComposeFile    string
	EnvFile        string
	ProjectName    string
	DefaultService string
	StateFile      string

	// Execute is the plan runner, injected so tests can drive the server without
	// touching Docker.
	Execute func(ctx context.Context, plan *protocol.Plan, options engine.Options, reporter engine.Reporter) error

	// ScheduleSelfRefresh is separated from Execute because it must run after the
	// run is marked complete and its status persisted.
	ScheduleSelfRefresh func(ctx context.Context, plan *protocol.Plan, options engine.Options, runID uint64, reporter engine.Reporter) (bool, error)
}

// Server exposes the updater's HTTP API.
type Server struct {
	config Config
	store  *Store
	ctx    context.Context
}

func New(ctx context.Context, config Config) *Server {
	if ctx == nil {
		ctx = context.Background()
	}
	if config.Execute == nil {
		config.Execute = engine.Execute
	}
	if config.ScheduleSelfRefresh == nil {
		config.ScheduleSelfRefresh = engine.ScheduleSelfRefresh
	}
	return &Server{config: config, store: NewStore(config.StateFile), ctx: ctx}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/events", s.handleEvents)
	mux.HandleFunc("/v1/update", s.handleUpdate)
	return mux
}

// authorized enforces the shared token.
//
// An empty configured token denies everything rather than allowing everything. The
// updater can recreate containers as root, so an unconfigured deployment must fail
// closed.
func (s *Server) authorized(r *http.Request) bool {
	if s.config.Token == "" {
		return false
	}
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = strings.TrimSpace(value[len("Bearer "):])
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(s.config.Token)) == 1
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	snapshot := s.store.Snapshot()
	health := protocol.Health{
		Status:          snapshot.Status,
		ProtocolVersion: protocol.ProtocolVersion,
		Events:          "/v1/events",
		MaxPlanVersion:  protocol.MaxPlanVersion,
	}
	if snapshot.Status == protocol.StatusFailed {
		health.Error = strings.TrimSpace(snapshot.Message)
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, s.store.Snapshot())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Without this nginx buffers the stream and the panel receives nothing until the
	// update finishes, which is indistinguishable from a hung update.
	w.Header().Set("X-Accel-Buffering", "no")

	replay, events, unsubscribe := s.store.Subscribe(parseLastEventID(r))
	defer unsubscribe()

	for _, event := range replay {
		if err := writeEvent(w, event); err != nil {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(eventHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case status := <-events:
			if err := writeEvent(w, status); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseLastEventID reads the resume point from either the SSE header or a query
// parameter. The header is the standard, but it cannot be set by EventSource, and
// the management API forwards it as a query parameter in that case.
func parseLastEventID(r *http.Request) uint64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("last_event_id"))
	}
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	// Authentication precedes the method check so an unauthenticated caller cannot
	// probe which methods an endpoint accepts.
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var request protocol.UpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	service := protocol.SanitizeServiceName(request.Service)
	if service == "" {
		service = s.config.DefaultService
	}
	if service == "" {
		http.Error(w, "missing target service", http.StatusBadRequest)
		return
	}

	plan, err := s.resolvePlan(request, service)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	runID, started := s.store.StartRun(service, request)
	if !started {
		http.Error(w, "update already running", http.StatusConflict)
		return
	}

	go s.runUpdate(runID, plan)

	writeJSON(w, http.StatusAccepted, protocol.UpdateAccepted{
		Status:  "accepted",
		Service: service,
		RunID:   runID,
	})
}

// resolvePlan returns the plan to execute, synthesizing one for applications that
// still send the flat protocol 2 request.
//
// The synthesized plan reproduces the historical behaviour exactly: pull the target
// service, then recreate it and wait for health. It deliberately does not invent
// dependency stages, because an application old enough to send a flat request is
// also old enough that guessing at its topology would be wrong.
func (s *Server) resolvePlan(request protocol.UpdateRequest, service string) (*protocol.Plan, error) {
	if request.Plan != nil {
		validated, err := request.Plan.Validate()
		if err != nil {
			return nil, err
		}
		return validated, nil
	}

	legacy := &protocol.Plan{
		PlanVersion: protocol.MinPlanVersion,
		Image:       request.Image,
		Tag:         request.Tag,
		Stages: []protocol.Stage{{
			ID:             "application",
			Services:       []string{service},
			WaitHealthy:    true,
			NoDeps:         true,
			RemoveOrphans:  true,
			TimeoutSeconds: 120,
			Weight:         2,
		}},
		Pull: []string{service},
	}
	return legacy.Validate()
}

func (s *Server) runUpdate(runID uint64, plan *protocol.Plan) {
	ctx, cancel := context.WithTimeout(s.ctx, updateCommandTimeout)
	defer cancel()

	reporter := s.store.Reporter(runID)
	envFile := s.resolveEnvFile()
	previousImage := engine.ReadConfiguredImage(envFile)

	if err := engine.PersistImage(ctx, envFile, plan.Image, plan.Tag, reporter); err != nil {
		message := "failed to update the env file: " + err.Error()
		if errors.Is(err, engine.ErrImageNotAllowed) {
			message = err.Error()
		}
		log.Print(message)
		s.store.Finish(runID, protocol.StatusFailed, protocol.StageFailed, "request_rejected", message)
		return
	}

	options := engine.Options{
		Docker: engine.Docker{
			ComposeFile: s.config.ComposeFile,
			EnvFile:     envFile,
			ProjectName: s.config.ProjectName,
		},
		ProjectDir: engine.ProjectDir(ctx, s.config.ComposeFile),
		SelfRunID:  runID,
	}

	if err := s.config.Execute(ctx, plan, options, reporter); err != nil {
		// Put the previous image back so a retry does not start from a half-applied
		// configuration, then report the original failure.
		err = engine.RestoreImage(ctx, envFile, previousImage, reporter, err)
		log.Printf("update failed: %v", err)
		s.store.Finish(runID, protocol.StatusFailed, protocol.StageFailed, "update_failed", err.Error())
		return
	}

	// The run is marked complete and persisted before the sidecar refresh is
	// scheduled. The refresh replaces this very process, so anything recorded after
	// it may never be written; the replacement restores this snapshot instead.
	s.store.Finish(runID, protocol.StatusCompleted, protocol.StageCompleted, "completed", "update completed")

	if _, err := s.config.ScheduleSelfRefresh(ctx, plan, options, runID, reporter); err != nil {
		// A failed self-refresh leaves an older sidecar running against a newer
		// application, which still works. Logging beats failing a successful update.
		log.Printf("updater sidecar refresh scheduling failed: %v", err)
		reporter.Log("stderr", "the updater sidecar could not be refreshed automatically: "+err.Error())
	}
}

// resolveEnvFile falls back to the compose file's sibling .env, matching what
// docker compose itself does when no env file is configured.
func (s *Server) resolveEnvFile() string {
	if envFile := strings.TrimSpace(s.config.EnvFile); envFile != "" {
		return envFile
	}
	if composeFile := strings.TrimSpace(s.config.ComposeFile); composeFile != "" {
		return filepath.Join(filepath.Dir(composeFile), ".env")
	}
	return ""
}

func writeEvent(w http.ResponseWriter, status protocol.Status) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if status.EventID > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", status.EventID); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "event: update\n"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
