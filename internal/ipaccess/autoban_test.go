package ipaccess

import (
	"context"
	"net"
	"testing"
	"time"
)

func testRegistry(t *testing.T, policy ProtectionPolicy) *Registry {
	t.Helper()
	registry := NewRegistry(NewStore(nil))
	normalized := policy.Normalized()
	registry.policy.Store(&normalized)
	registry.matcher.Store(NewMatcher(nil, normalized.Lockdown, time.Now()))
	return registry
}

func trustedAddress(ip string) ClientAddress {
	return ClientAddress{IP: net.ParseIP(ip), Raw: ip, Trusted: true}
}

func TestAutoBanObserveModeDoesNotWriteRules(t *testing.T) {
	// The shipped default must never ban on its own: an auto-ban firing on a
	// threshold nobody has watched against real traffic is a self-inflicted
	// outage.
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 3
	registry := testRegistry(t, policy)

	var outcome AutoBanOutcome
	for i := 0; i < 3; i++ {
		outcome = registry.AutoBan().RecordFailure(context.Background(), trustedAddress("203.0.113.5"), "test")
	}
	if !outcome.Triggered {
		t.Fatal("threshold was reached but the rule did not trigger")
	}
	if outcome.Enforced {
		t.Error("observe mode wrote a ban rule")
	}
}

func TestAutoBanBelowThresholdDoesNothing(t *testing.T) {
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 5
	registry := testRegistry(t, policy)

	for i := 0; i < 4; i++ {
		if outcome := registry.AutoBan().RecordFailure(context.Background(), trustedAddress("203.0.113.6"), "test"); outcome.Triggered {
			t.Fatalf("triggered after %d failures, threshold is 5", i+1)
		}
	}
}

func TestAutoBanSkipsAllowListedSource(t *testing.T) {
	// An operator's explicit allow must outrank the engine's judgement, or the
	// office IP gets banned by the engine the operator was trying to protect it
	// from.
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 2
	registry := testRegistry(t, policy)
	rule := Rule{ID: "allow", CIDR: "203.0.113.7/32", Effect: EffectAllow, Enabled: true}
	registry.matcher.Store(NewMatcher([]Rule{rule}, false, time.Now()))

	for i := 0; i < 5; i++ {
		if outcome := registry.AutoBan().RecordFailure(context.Background(), trustedAddress("203.0.113.7"), "test"); outcome.Triggered {
			t.Fatal("an allow-listed source was proposed for a ban")
		}
	}
}

func TestAutoBanSkipsUntrustedAndLoopback(t *testing.T) {
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 1
	registry := testRegistry(t, policy)

	untrusted := ClientAddress{IP: net.ParseIP("172.17.0.1"), Raw: "172.17.0.1", Trusted: false}
	if outcome := registry.AutoBan().RecordFailure(context.Background(), untrusted, "test"); outcome.Triggered {
		t.Error("banned an untrusted address: that is the proxy, and every client behind it")
	}
	if outcome := registry.AutoBan().RecordFailure(context.Background(), trustedAddress("127.0.0.1"), "test"); outcome.Triggered {
		t.Error("banned loopback: the operator's own channel")
	}
}

func TestAutoBanModeOffDisablesEngine(t *testing.T) {
	policy := DefaultPolicy()
	policy.AutoBan.Mode = AutoBanOff
	policy.AutoBan.FailureThreshold = 1
	registry := testRegistry(t, policy)

	if outcome := registry.AutoBan().RecordFailure(context.Background(), trustedAddress("203.0.113.8"), "test"); outcome.Triggered {
		t.Error("engine acted while switched off")
	}
}

func TestAutoBanDoesNotRetriggerWhileBanned(t *testing.T) {
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 2
	registry := testRegistry(t, policy)
	address := trustedAddress("203.0.113.9")

	registry.AutoBan().RecordFailure(context.Background(), address, "test")
	first := registry.AutoBan().RecordFailure(context.Background(), address, "test")
	if !first.Triggered {
		t.Fatal("expected the second failure to trigger")
	}
	for i := 0; i < 5; i++ {
		if again := registry.AutoBan().RecordFailure(context.Background(), address, "test"); again.Triggered {
			t.Error("re-triggered while a ban was already in force; one offender should produce one decision")
		}
	}
}

func TestBanDurationEscalatesAndClamps(t *testing.T) {
	policy := AutoBanPolicy{BanMinutes: 60, MaxBanMinutes: 240}
	cases := []struct {
		repeats int
		want    time.Duration
	}{
		{0, 60 * time.Minute},
		{1, 120 * time.Minute},
		{2, 240 * time.Minute},
		{9, 240 * time.Minute},
	}
	for _, tc := range cases {
		if got := policy.BanDuration(tc.repeats); got != tc.want {
			t.Errorf("BanDuration(%d) = %v, want %v", tc.repeats, got, tc.want)
		}
	}
}

func TestPolicyNormalizedRejectsZeroThreshold(t *testing.T) {
	// A zero threshold would ban the first client to mistype a password.
	policy := ProtectionPolicy{AutoBan: AutoBanPolicy{Mode: AutoBanEnforce}}.Normalized()
	if policy.AutoBan.FailureThreshold <= 0 {
		t.Error("threshold was left at zero")
	}
	if policy.AutoBan.WindowSeconds <= 0 || policy.AutoBan.BanMinutes <= 0 {
		t.Error("window or ban duration was left at zero")
	}
}
