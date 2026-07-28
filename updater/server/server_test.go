package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clirelay.local/updater/engine"
	"clirelay.local/updater/protocol"
)

const testToken = "test-token"

func newTestServer(t *testing.T, config Config) (*Server, *httptest.Server) {
	t.Helper()
	if config.Token == "" {
		config.Token = testToken
	}
	if config.StateFile == "" {
		config.StateFile = t.TempDir() + "/status.json"
	}
	if config.DefaultService == "" {
		config.DefaultService = "cli-proxy-api"
	}
	if config.Execute == nil {
		config.Execute = func(context.Context, *protocol.Plan, engine.Options, engine.Reporter) error { return nil }
	}
	if config.ScheduleSelfRefresh == nil {
		config.ScheduleSelfRefresh = func(context.Context, *protocol.Plan, engine.Options, uint64, engine.Reporter) (bool, error) {
			return false, nil
		}
	}
	updater := New(context.Background(), config)
	httpServer := httptest.NewServer(updater.Handler())
	t.Cleanup(httpServer.Close)
	// A run continues in its own goroutine after the request returns. Letting it
	// settle before the temp directory is torn down keeps it from writing the state
	// file into a directory the test framework has already removed.
	t.Cleanup(func() {
		waitFor(t, func() bool { return !updater.store.Snapshot().IsRunning() })
	})
	return updater, httpServer
}

func authRequest(t *testing.T, method string, url string, body string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

// TestEmptyTokenDeniesEverything preserves an existing security property: the
// updater can recreate containers as root, so an unconfigured deployment must fail
// closed rather than open.
func TestEmptyTokenDeniesEverything(t *testing.T) {
	_, httpServer := newTestServer(t, Config{Token: " "})

	for _, path := range []string{"/v1/health", "/v1/status", "/v1/events", "/v1/update"} {
		resp, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with an unconfigured token = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestHealthAdvertisesPlanSupport(t *testing.T) {
	_, httpServer := newTestServer(t, Config{})

	resp, err := http.DefaultClient.Do(authRequest(t, http.MethodGet, httpServer.URL+"/v1/health", ""))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()

	var health protocol.Health
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.ProtocolVersion != protocol.ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", health.ProtocolVersion, protocol.ProtocolVersion)
	}
	if health.MaxPlanVersion != protocol.MaxPlanVersion {
		t.Errorf("MaxPlanVersion = %d, want %d", health.MaxPlanVersion, protocol.MaxPlanVersion)
	}
	if health.Events != "/v1/events" {
		t.Errorf("Events = %q, want /v1/events", health.Events)
	}
}

// TestLegacyRequestSynthesizesAPlan covers an application that has not been updated
// yet talking to a refreshed sidecar — the normal state of affairs mid-rollout.
func TestLegacyRequestSynthesizesAPlan(t *testing.T) {
	var captured *protocol.Plan
	var wg sync.WaitGroup
	wg.Add(1)

	_, httpServer := newTestServer(t, Config{
		Execute: func(_ context.Context, plan *protocol.Plan, _ engine.Options, _ engine.Reporter) error {
			captured = plan
			wg.Done()
			return nil
		},
	})

	body := `{"service":"cli-proxy-api","image":"ghcr.io/kittors/clirelay","tag":"dev"}`
	resp, err := http.DefaultClient.Do(authRequest(t, http.MethodPost, httpServer.URL+"/v1/update", body))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	wg.Wait()

	if captured == nil {
		t.Fatal("no plan was synthesized")
	}
	if len(captured.Stages) != 1 || captured.Stages[0].Services[0] != "cli-proxy-api" {
		t.Fatalf("synthesized plan targets the wrong services: %+v", captured.Stages)
	}
	if !captured.Stages[0].WaitHealthy {
		t.Error("the synthesized plan must still wait for health, as the old updater did")
	}
	if len(captured.Pull) != 1 || captured.Pull[0] != "cli-proxy-api" {
		t.Errorf("synthesized pull list = %v", captured.Pull)
	}
}

func TestPlanRequestIsExecutedAsWritten(t *testing.T) {
	var captured *protocol.Plan
	var wg sync.WaitGroup
	wg.Add(1)

	_, httpServer := newTestServer(t, Config{
		Execute: func(_ context.Context, plan *protocol.Plan, _ engine.Options, _ engine.Reporter) error {
			captured = plan
			wg.Done()
			return nil
		},
	})

	body := `{
		"service": "cli-proxy-api",
		"image": "ghcr.io/kittors/clirelay",
		"tag": "dev",
		"plan": {
			"plan_version": 1,
			"image": "ghcr.io/kittors/clirelay",
			"tag": "dev",
			"pull": ["cli-proxy-api"],
			"stages": [
				{"id": "dependencies", "services": ["postgres", "redis"], "wait_healthy": true, "optional": true},
				{"id": "application", "services": ["cli-proxy-api"], "wait_healthy": true, "no_deps": true, "weight": 2}
			]
		}
	}`
	resp, err := http.DefaultClient.Do(authRequest(t, http.MethodPost, httpServer.URL+"/v1/update", body))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	resp.Body.Close()
	wg.Wait()

	if len(captured.Stages) != 2 {
		t.Fatalf("stage count = %d, want 2", len(captured.Stages))
	}
	if captured.Stages[0].ID != "dependencies" || captured.Stages[1].ID != "application" {
		t.Error("stage order was not preserved")
	}
}

// TestUnsupportedPlanVersionIsRejectedUpFront makes an application newer than its
// sidecar fail before anything is touched, rather than half-applying an update.
func TestUnsupportedPlanVersionIsRejectedUpFront(t *testing.T) {
	executed := false
	_, httpServer := newTestServer(t, Config{
		Execute: func(context.Context, *protocol.Plan, engine.Options, engine.Reporter) error {
			executed = true
			return nil
		},
	})

	body := `{"service":"app","plan":{"plan_version":99,"image":"ghcr.io/kittors/clirelay","tag":"dev","stages":[{"id":"a","services":["app"]}]}}`
	resp, err := http.DefaultClient.Do(authRequest(t, http.MethodPost, httpServer.URL+"/v1/update", body))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if executed {
		t.Error("an unsupported plan must not reach the engine")
	}
}

func TestConcurrentUpdateIsRejected(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	_, httpServer := newTestServer(t, Config{
		Execute: func(context.Context, *protocol.Plan, engine.Options, engine.Reporter) error {
			close(started)
			<-release
			return nil
		},
	})
	defer close(release)

	body := `{"service":"cli-proxy-api","image":"ghcr.io/kittors/clirelay","tag":"dev"}`
	first, err := http.DefaultClient.Do(authRequest(t, http.MethodPost, httpServer.URL+"/v1/update", body))
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	first.Body.Close()
	<-started

	second, err := http.DefaultClient.Do(authRequest(t, http.MethodPost, httpServer.URL+"/v1/update", body))
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Errorf("second update = %d, want 409", second.StatusCode)
	}
}

// TestEventsResumeFromLastEventID is the fix for the panel losing progress when the
// application container is recreated: a reconnect must deliver what was missed
// rather than dropping the client into a hole.
func TestEventsResumeFromLastEventID(t *testing.T) {
	updater, httpServer := newTestServer(t, Config{})

	runID, ok := updater.store.StartRun("cli-proxy-api", protocol.UpdateRequest{})
	if !ok {
		t.Fatal("StartRun refused")
	}
	reporter := updater.store.Reporter(runID)
	reporter.Progress(engine.ProgressUpdate{Stage: protocol.StagePulling, Percent: 10})
	afterFirst := updater.store.Snapshot().EventID
	reporter.Progress(engine.ProgressUpdate{Stage: protocol.StagePulling, Percent: 55})
	updater.store.Finish(runID, protocol.StatusCompleted, protocol.StageCompleted, "completed", "done")

	req := authRequest(t, http.MethodGet, httpServer.URL+"/v1/events", "")
	req.Header.Set("Last-Event-ID", strconv.FormatUint(afterFirst, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()

	events := readEvents(t, resp, 2)
	if len(events) < 2 {
		t.Fatalf("expected the two missed events to be replayed, got %d", len(events))
	}
	for _, event := range events {
		if event.EventID <= afterFirst {
			t.Errorf("replayed an event the client already had: %d <= %d", event.EventID, afterFirst)
		}
	}
	if last := events[len(events)-1]; last.Status != protocol.StatusCompleted {
		t.Errorf("final replayed status = %q, want completed", last.Status)
	}
}

// TestEventsWithoutResumeSendCurrentSnapshot covers a fresh connection: it needs
// enough state to render immediately, not just future deltas.
func TestEventsWithoutResumeSendCurrentSnapshot(t *testing.T) {
	updater, httpServer := newTestServer(t, Config{})
	runID, _ := updater.store.StartRun("cli-proxy-api", protocol.UpdateRequest{})
	updater.store.Finish(runID, protocol.StatusCompleted, protocol.StageCompleted, "completed", "done")

	resp, err := http.DefaultClient.Do(authRequest(t, http.MethodGet, httpServer.URL+"/v1/events", ""))
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()

	events := readEvents(t, resp, 1)
	if len(events) == 0 {
		t.Fatal("a fresh subscriber received no snapshot")
	}
	if events[0].Status != protocol.StatusCompleted {
		t.Errorf("snapshot status = %q, want completed", events[0].Status)
	}
}

// TestFarBehindClientGetsFullResync guards the case where a client was gone longer
// than the replay window: it must resynchronize instead of receiving nothing.
func TestFarBehindClientGetsFullResync(t *testing.T) {
	updater, httpServer := newTestServer(t, Config{})
	runID, _ := updater.store.StartRun("cli-proxy-api", protocol.UpdateRequest{})
	reporter := updater.store.Reporter(runID)
	for index := range replayBufferSize + 50 {
		reporter.Progress(engine.ProgressUpdate{Stage: protocol.StagePulling, Percent: float64(index % 100)})
	}

	req := authRequest(t, http.MethodGet, httpServer.URL+"/v1/events", "")
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()

	events := readEvents(t, resp, 1)
	if len(events) != 1 {
		t.Fatalf("a far-behind client should get exactly one resync snapshot, got %d", len(events))
	}
	if events[0].EventID != updater.store.Snapshot().EventID {
		t.Error("the resync snapshot is not the current state")
	}
}

// TestCompletedStatusIsPersistedBeforeSelfRefresh pins the ordering that makes a
// completed update survive the sidecar replacing itself.
func TestCompletedStatusIsPersistedBeforeSelfRefresh(t *testing.T) {
	stateFile := t.TempDir() + "/status.json"
	persistedAtRefresh := ""

	_, httpServer := newTestServer(t, Config{
		StateFile: stateFile,
		ScheduleSelfRefresh: func(context.Context, *protocol.Plan, engine.Options, uint64, engine.Reporter) (bool, error) {
			data, _ := readFile(stateFile)
			persistedAtRefresh = data
			return true, nil
		},
	})

	body := `{"service":"cli-proxy-api","image":"ghcr.io/kittors/clirelay","tag":"dev"}`
	resp, err := http.DefaultClient.Do(authRequest(t, http.MethodPost, httpServer.URL+"/v1/update", body))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	resp.Body.Close()

	waitFor(t, func() bool { return strings.Contains(persistedAtRefresh, protocol.StatusCompleted) })
	if !strings.Contains(persistedAtRefresh, protocol.StatusCompleted) {
		t.Errorf("the completed status was not on disk before the self-refresh; got %q", persistedAtRefresh)
	}
}

// TestInterruptedRunIsRecoveredAsFailed covers the updater dying mid-update: the
// replacement must not leave the panel waiting on a run that can never finish.
func TestInterruptedRunIsRecoveredAsFailed(t *testing.T) {
	stateFile := t.TempDir() + "/status.json"
	interrupted := protocol.Status{
		RunID:   7,
		EventID: 42,
		Status:  protocol.StatusRunning,
		Stage:   protocol.StagePulling,
	}
	writeJSONFile(t, stateFile, interrupted)

	store := NewStore(stateFile)
	snapshot := store.Snapshot()
	if snapshot.Status != protocol.StatusFailed {
		t.Errorf("status = %q, want failed", snapshot.Status)
	}
	if snapshot.MessageCode != "updater_restarted" {
		t.Errorf("message code = %q, want updater_restarted", snapshot.MessageCode)
	}
	if snapshot.EventID <= interrupted.EventID {
		t.Errorf("event id must keep advancing across restarts: %d <= %d", snapshot.EventID, interrupted.EventID)
	}
}

// TestSelfRefreshRunIsRecoveredAsCompleted is the counterpart: an update that had
// already reached its final stage was interrupted by its own success, and must not
// be reported to the user as a failure.
func TestSelfRefreshRunIsRecoveredAsCompleted(t *testing.T) {
	stateFile := t.TempDir() + "/status.json"
	writeJSONFile(t, stateFile, protocol.Status{
		RunID:           7,
		EventID:         42,
		Status:          protocol.StatusRunning,
		Stage:           protocol.StageFinalizing,
		ProgressPercent: 100,
	})

	snapshot := NewStore(stateFile).Snapshot()
	if snapshot.Status != protocol.StatusCompleted {
		t.Errorf("status = %q, want completed", snapshot.Status)
	}
	if snapshot.ProgressPercent != 100 {
		t.Errorf("percent = %v, want 100", snapshot.ProgressPercent)
	}
}

func TestProgressPercentNeverRegresses(t *testing.T) {
	store := NewStore(t.TempDir() + "/status.json")
	runID, _ := store.StartRun("app", protocol.UpdateRequest{})
	reporter := store.Reporter(runID)

	reporter.Progress(engine.ProgressUpdate{Stage: protocol.StagePulling, Percent: 60})
	reporter.Progress(engine.ProgressUpdate{Stage: protocol.StagePulling, Percent: 20})

	if percent := store.Snapshot().ProgressPercent; percent != 60 {
		t.Errorf("percent = %v, want it held at 60", percent)
	}
}

func readEvents(t *testing.T, resp *http.Response, want int) []protocol.Status {
	t.Helper()
	var events []protocol.Status
	scanner := bufio.NewScanner(resp.Body)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for scanner.Scan() {
			line := scanner.Text()
			payload, found := strings.CutPrefix(line, "data: ")
			if !found {
				continue
			}
			var status protocol.Status
			if err := json.Unmarshal([]byte(payload), &status); err != nil {
				continue
			}
			events = append(events, status)
			if len(events) >= want {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out reading events")
	}
	return events
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
