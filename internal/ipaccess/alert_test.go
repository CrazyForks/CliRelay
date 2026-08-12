package ipaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAlertCooldownSuppressesRepeatsPerSource(t *testing.T) {
	// Under attack the engine decides about one source many times. A channel that
	// relays every decision is a channel operators mute, which is worse than none.
	a := newAlerter()
	now := time.Now()
	cooldown := 30 * time.Minute

	if !a.shouldSend("1.2.3.4/32", cooldown, now) {
		t.Fatal("first alert for a source must be delivered")
	}
	if a.shouldSend("1.2.3.4/32", cooldown, now.Add(29*time.Minute)) {
		t.Error("repeat inside the cooldown was not suppressed")
	}
	// A different source is unrelated and must not be suppressed by the first.
	if !a.shouldSend("5.6.7.8/32", cooldown, now.Add(time.Minute)) {
		t.Error("a different source was suppressed by another source's cooldown")
	}
	if !a.shouldSend("1.2.3.4/32", cooldown, now.Add(31*time.Minute)) {
		t.Error("alert after the cooldown expired was suppressed")
	}
}

func TestAlertDedupTableStaysBounded(t *testing.T) {
	// An attacker minting sources must not be able to grow this table without
	// limit just by failing logins from new addresses.
	a := newAlerter()
	now := time.Now()
	cooldown := time.Hour
	for i := 0; i < maxTrackedAlertKeys+500; i++ {
		a.shouldSend(string(rune(i%128))+"-"+time.Duration(i).String(), cooldown, now)
	}
	a.mu.Lock()
	size := len(a.last)
	a.mu.Unlock()
	if size > maxTrackedAlertKeys {
		t.Errorf("dedup table grew to %d, cap is %d", size, maxTrackedAlertKeys)
	}
}

func TestNotifyBanSkipsObserveUnlessOptedIn(t *testing.T) {
	var (
		mu       sync.Mutex
		received []AlertEvent
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event AlertEvent
		_ = json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	registry := NewRegistry(NewStore(nil))
	policy := DefaultPolicy()
	policy.Alert = AlertPolicy{WebhookURL: server.URL, CooldownMinutes: 30}
	registry.SetPolicy(context.Background(), policy)

	// Observe-mode decision: nothing was blocked, so it is opt-in.
	registry.notifyBan(AutoBanOutcome{Triggered: true, Enforced: false, CIDR: "1.2.3.4/32"}, "test")
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	countAfterObserve := len(received)
	mu.Unlock()
	if countAfterObserve != 0 {
		t.Fatalf("observe-mode decision delivered %d alerts without opting in", countAfterObserve)
	}

	// An enforced ban always alerts.
	registry.notifyBan(AutoBanOutcome{Triggered: true, Enforced: true, CIDR: "5.6.7.8/32"}, "test")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("enforced ban delivered %d alerts, want 1", len(received))
	}
	if received[0].Event != "ip_access.auto_ban" || received[0].CIDR != "5.6.7.8/32" {
		t.Errorf("unexpected payload: %+v", received[0])
	}
}

func TestNotifyBanOptsIntoObserveAlerts(t *testing.T) {
	var (
		mu       sync.Mutex
		received []AlertEvent
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event AlertEvent
		_ = json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	}))
	defer server.Close()

	registry := NewRegistry(NewStore(nil))
	policy := DefaultPolicy()
	policy.Alert = AlertPolicy{WebhookURL: server.URL, NotifyObserve: true, CooldownMinutes: 30}
	registry.SetPolicy(context.Background(), policy)

	registry.notifyBan(AutoBanOutcome{Triggered: true, Enforced: false, CIDR: "1.2.3.4/32"}, "test")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(received)
		mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("got %d alerts, want 1 once observe alerts are opted into", len(received))
	}
	// Observe alerts are labelled differently so a rule can route them elsewhere.
	if received[0].Event != "ip_access.auto_ban_observed" {
		t.Errorf("event = %q, want the observed variant", received[0].Event)
	}
}

func TestNotifyBanIsANoOpWithoutWebhook(t *testing.T) {
	registry := NewRegistry(NewStore(nil))
	// Must not panic or block when no channel is configured.
	registry.notifyBan(AutoBanOutcome{Triggered: true, Enforced: true, CIDR: "1.2.3.4/32"}, "test")
}

func TestUnreachableWebhookDoesNotBlockTheDecision(t *testing.T) {
	// Alerting must never become the slowest part of rejecting a request.
	registry := NewRegistry(NewStore(nil))
	policy := DefaultPolicy()
	policy.Alert = AlertPolicy{WebhookURL: "http://127.0.0.1:1/unreachable", CooldownMinutes: 1}
	registry.SetPolicy(context.Background(), policy)

	start := time.Now()
	registry.notifyBan(AutoBanOutcome{Triggered: true, Enforced: true, CIDR: "1.2.3.4/32"}, "test")
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("notifyBan blocked for %s; delivery must be detached", elapsed)
	}
}

func TestAttemptRetentionIsClampedNotTrusted(t *testing.T) {
	// The attempt table grows with attack volume, so unbounded retention is a
	// disk-exhaustion path an attacker controls.
	if got := (ProtectionPolicy{AttemptRetentionDays: 0}).Normalized().AttemptRetentionDays; got != DefaultAttemptRetentionDays {
		t.Errorf("zero retention = %d, want the default %d", got, DefaultAttemptRetentionDays)
	}
	if got := (ProtectionPolicy{AttemptRetentionDays: 99999}).Normalized().AttemptRetentionDays; got != maxAttemptRetentionDays {
		t.Errorf("oversized retention = %d, want the cap %d", got, maxAttemptRetentionDays)
	}
	if got := (ProtectionPolicy{AttemptRetentionDays: 7}).Normalized().AttemptRetentionDays; got != 7 {
		t.Errorf("valid retention = %d, want it preserved", got)
	}
}
