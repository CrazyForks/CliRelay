package ipaccess

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func mustRule(t *testing.T, cidr string, effect Effect) Rule {
	t.Helper()
	normalized, family, err := NormalizeCIDR(cidr)
	if err != nil {
		t.Fatalf("NormalizeCIDR(%q): %v", cidr, err)
	}
	return Rule{ID: cidr, CIDR: normalized, Family: family, Effect: effect, Enabled: true}
}

func TestNormalizeCIDRClearsHostBits(t *testing.T) {
	// Without this the same network can be stored twice under different spellings
	// and the uniqueness constraint never fires.
	got, family, err := NormalizeCIDR("1.2.3.5/24")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "1.2.3.0/24" || family != 4 {
		t.Fatalf("got %q family %d, want 1.2.3.0/24 family 4", got, family)
	}
}

func TestNormalizeCIDRBareAddressBecomesHostRoute(t *testing.T) {
	got, _, err := NormalizeCIDR("203.0.113.7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "203.0.113.7/32" {
		t.Fatalf("got %q, want 203.0.113.7/32", got)
	}
}

func TestNormalizeCIDRRejectsSelfDestructiveInput(t *testing.T) {
	for _, input := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/4", "127.0.0.1", "::1", "", "not-an-ip"} {
		if _, _, err := NormalizeCIDR(input); err == nil {
			t.Errorf("NormalizeCIDR(%q) accepted input that should be rejected", input)
		}
	}
}

func TestMatcherAllowBeatsDeny(t *testing.T) {
	// A specific exception must override a broad ban, or banning a /16 makes the
	// one office machine inside it unreachable with no way back short of deleting
	// the ban entirely.
	matcher := NewMatcher([]Rule{
		mustRule(t, "1.2.0.0/16", EffectDeny),
		mustRule(t, "1.2.3.4/32", EffectAllow),
	}, false, time.Now())

	if decision, _ := matcher.Match(net.ParseIP("1.2.3.4")); decision != DecisionAllow {
		t.Errorf("allowed host inside denied range: got %v, want allow", decision)
	}
	if decision, _ := matcher.Match(net.ParseIP("1.2.9.9")); decision != DecisionDeny {
		t.Errorf("other host in denied range: got %v, want deny", decision)
	}
	if decision, _ := matcher.Match(net.ParseIP("9.9.9.9")); decision != DecisionNeutral {
		t.Errorf("unrelated host: got %v, want neutral", decision)
	}
}

func TestMatcherLongestPrefixWins(t *testing.T) {
	matcher := NewMatcher([]Rule{
		mustRule(t, "10.0.0.0/8", EffectDeny),
		mustRule(t, "10.1.2.0/24", EffectAllow),
	}, false, time.Now())
	if decision, _ := matcher.Match(net.ParseIP("10.1.2.99")); decision != DecisionAllow {
		t.Errorf("got %v, want allow from the more specific rule", decision)
	}
}

func TestMatcherIPv6MatchesBySixtyFour(t *testing.T) {
	matcher := NewMatcher([]Rule{mustRule(t, "2001:db8:1:2::/64", EffectDeny)}, false, time.Now())
	if decision, _ := matcher.Match(net.ParseIP("2001:db8:1:2::dead")); decision != DecisionDeny {
		t.Errorf("got %v, want deny for an address inside the /64", decision)
	}
	if decision, _ := matcher.Match(net.ParseIP("2001:db8:1:3::1")); decision != DecisionNeutral {
		t.Errorf("got %v, want neutral for a neighbouring /64", decision)
	}
}

func TestMatcherSkipsExpiredAndDisabled(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	expired := mustRule(t, "5.5.5.5/32", EffectDeny)
	expired.ExpiresAt = &past
	disabled := mustRule(t, "6.6.6.6/32", EffectDeny)
	disabled.Enabled = false

	matcher := NewMatcher([]Rule{expired, disabled}, false, time.Now())
	if decision, _ := matcher.Match(net.ParseIP("5.5.5.5")); decision != DecisionNeutral {
		t.Errorf("expired rule still applied: got %v", decision)
	}
	if decision, _ := matcher.Match(net.ParseIP("6.6.6.6")); decision != DecisionNeutral {
		t.Errorf("disabled rule still applied: got %v", decision)
	}
}

func TestMatcherLockdownDeniesUnlisted(t *testing.T) {
	matcher := NewMatcher([]Rule{mustRule(t, "1.2.3.4/32", EffectAllow)}, true, time.Now())
	if decision, _ := matcher.Match(net.ParseIP("1.2.3.4")); decision != DecisionAllow {
		t.Errorf("listed address under lockdown: got %v, want allow", decision)
	}
	if decision, _ := matcher.Match(net.ParseIP("1.2.3.5")); decision != DecisionDeny {
		t.Errorf("unlisted address under lockdown: got %v, want deny", decision)
	}
}

func TestResolveMarksRelayedRequestUntrustedWithoutTrustedProxies(t *testing.T) {
	// This is the production state that made the whole feature inert: nginx sets
	// X-Forwarded-For, trusted-proxies is empty, so the resolved address is the
	// proxy's and stands for every client behind it.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	untrusted := Resolve(req, "172.17.0.1", false)
	if untrusted.Trusted {
		t.Error("relayed request with no trusted-proxies was treated as trustworthy")
	}
	if untrusted.RelayHeader == "" {
		t.Error("relay header was not reported")
	}

	trusted := Resolve(req, "203.0.113.9", true)
	if !trusted.Trusted {
		t.Error("relayed request with trusted-proxies configured should be trusted")
	}
	if trusted.Raw != "203.0.113.9" {
		t.Errorf("got %q, want the framework-resolved client address", trusted.Raw)
	}
}

func TestResolveIgnoresForwardingHeaderAsAddress(t *testing.T) {
	// The header is attacker-controlled; only the framework-resolved address may
	// be used, otherwise a banned client renames itself out of its ban.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	address := Resolve(req, "198.51.100.7", true)
	if address.Raw != "198.51.100.7" {
		t.Errorf("got %q, want the resolved address rather than the header value", address.Raw)
	}
}

func TestRegistryDoesNotEnforceUntrustedAddress(t *testing.T) {
	registry := NewRegistry(NewStore(nil))
	registry.matcher.Store(NewMatcher([]Rule{mustRule(t, "203.0.113.9/32", EffectDeny)}, false, time.Now()))

	verdict := registry.Evaluate(ClientAddress{IP: net.ParseIP("203.0.113.9"), Raw: "203.0.113.9", Trusted: false})
	if verdict.Enforced {
		t.Error("verdict was enforced for an untrusted address")
	}
	if !verdict.Allowed() {
		t.Error("an unenforceable verdict must fail open, never deny")
	}
}

func TestRegistryAlwaysAdmitsLoopback(t *testing.T) {
	registry := NewRegistry(NewStore(nil))
	policy := DefaultPolicy()
	policy.Lockdown = true
	registry.policy.Store(&policy)
	registry.matcher.Store(NewMatcher(nil, true, time.Now()))

	verdict := registry.Evaluate(ClientAddress{IP: net.ParseIP("127.0.0.1"), Raw: "127.0.0.1", Trusted: true})
	if !verdict.Allowed() {
		t.Error("loopback was denied; the operator would lose access to their own host")
	}
}

func TestBanCIDRCollapsesIPv6ToSixtyFour(t *testing.T) {
	if got := BanCIDR(net.ParseIP("2001:db8:1:2::5")); got != "2001:db8:1:2::/64" {
		t.Errorf("got %q, want the /64 so the next address in the block is covered", got)
	}
	if got := BanCIDR(net.ParseIP("192.0.2.5")); got != "192.0.2.5/32" {
		t.Errorf("got %q, want a host route for IPv4", got)
	}
}

func TestProtectedAddressesSurviveEveryRule(t *testing.T) {
	// The interesting failure here is self-inflicted: a rule covering the host's
	// own address or its reverse proxy takes the deployment offline instead of
	// blocking an attacker, so the list must not be able to express it.
	registry := NewRegistry(NewStore(nil))
	registry.SetProtectedAddresses([]string{"10.0.0.5"}, []string{"104.194.69.137"}, nil)
	registry.matcher.Store(NewMatcher([]Rule{
		mustRule(t, "10.0.0.0/8", EffectDeny),
		mustRule(t, "104.194.69.137/32", EffectDeny),
	}, true, time.Now()))

	for _, ip := range []string{"10.0.0.5", "104.194.69.137"} {
		verdict := registry.Evaluate(ClientAddress{IP: net.ParseIP(ip), Raw: ip, Trusted: true})
		if !verdict.Allowed() || !verdict.Enforced || verdict.Decision != DecisionAllow {
			t.Errorf("%s: got %v (enforced=%t), want an enforced allow despite the deny rules", ip, verdict.Decision, verdict.Enforced)
		}
	}
	// Protection is per-address, not per-range: a neighbour inside the same denied
	// /8 must not inherit it.
	if _, ok := registry.Protected(net.ParseIP("10.0.0.6")); ok {
		t.Error("protection leaked to an unprotected address in the same range")
	}
}

func TestProtectedAddressesAreNotAutoBanned(t *testing.T) {
	policy := DefaultPolicy()
	policy.AutoBan.FailureThreshold = 2
	registry := testRegistry(t, policy)
	registry.SetProtectedAddresses(nil, []string{"104.194.69.137"}, nil)

	for i := 0; i < 5; i++ {
		outcome := registry.AutoBan().RecordFailure(context.Background(), trustedAddress("104.194.69.137"), "test")
		if outcome.Triggered {
			t.Fatal("the reverse proxy was proposed for an automatic ban")
		}
	}
}

func TestProxyHostAddressesKeepsOnlyLiterals(t *testing.T) {
	// A hostname is skipped rather than resolved: resolving at startup bakes in
	// one answer and silently protects the wrong address after a DNS change.
	got := ProxyHostAddresses([]string{
		"http://203.0.113.9:8080",
		"socks5://proxy.internal:1080",
		"not a url",
		"",
	})
	if len(got) != 1 || got[0] != "203.0.113.9" {
		t.Errorf("got %v, want [203.0.113.9]", got)
	}
}

func TestResolveForwardedWalksChainAgainstDeclaredProxies(t *testing.T) {
	// The rightmost hops were appended by infrastructure we control; everything
	// left of the first untrusted hop is attacker-supplied and must be discarded.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.7, 10.0.0.2")

	matcher := newTrustedProxyMatcher([]string{"10.0.0.0/24"})
	addr := resolveForwarded(req, matcher)
	if !addr.Trusted {
		t.Fatal("chain resolved through declared proxies should be trusted")
	}
	if addr.Raw != "203.0.113.7" {
		t.Errorf("got %q, want the first hop no trusted proxy vouched for", addr.Raw)
	}
}

func TestResolveForwardedIgnoresHeaderFromUndeclaredPeer(t *testing.T) {
	// A direct client claiming upstream hops is unverifiable, so the claim is
	// dropped rather than believed — otherwise a banned client renames itself.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.9:44000"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	addr := resolveForwarded(req, newTrustedProxyMatcher([]string{"10.0.0.0/24"}))
	if addr.Raw != "203.0.113.9" || !addr.Trusted {
		t.Errorf("got %q trusted=%t, want the peer address treated as the client", addr.Raw, addr.Trusted)
	}
}

func TestResolveForwardedUntrustedWhenRelayedWithNoDeclaredProxy(t *testing.T) {
	// This is the production state: nginx relays, nothing is declared, so the
	// address is the proxy's and stands for everyone behind it.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "104.194.69.137:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	addr := resolveForwarded(req, newTrustedProxyMatcher(nil))
	if addr.Trusted {
		t.Error("relayed request with no declared proxy must not be trusted")
	}
	if addr.Raw != "104.194.69.137" {
		t.Errorf("got %q, want the proxy address", addr.Raw)
	}
}

func TestResolveForwardedAllHopsTrustedIsNotAClient(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "10.0.0.2, 10.0.0.3")

	addr := resolveForwarded(req, newTrustedProxyMatcher([]string{"10.0.0.0/24"}))
	if addr.Trusted {
		t.Error("a chain of only proxies identifies no client and must not be trusted")
	}
}

func TestPolicyNormalizesAndDeduplicatesTrustedProxies(t *testing.T) {
	policy := ProtectionPolicy{TrustedProxies: []string{
		" 104.194.69.137 ", "104.194.69.137", "10.0.0.0/24", "not-an-ip", "",
	}}.Normalized()
	want := []string{"104.194.69.137/32", "10.0.0.0/24"}
	if len(policy.TrustedProxies) != len(want) {
		t.Fatalf("got %v, want %v", policy.TrustedProxies, want)
	}
	for i := range want {
		if policy.TrustedProxies[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, policy.TrustedProxies[i], want[i])
		}
	}
}

func TestRegistryPrefersStoredProxiesOverConfig(t *testing.T) {
	registry := NewRegistry(NewStore(nil))
	registry.SetConfiguredProxies([]string{"10.0.0.0/24"})
	if proxies, source := registry.TrustedProxies(); source != "config" || len(proxies) != 1 {
		t.Fatalf("got %v from %q, want the config fallback", proxies, source)
	}
	registry.SetPolicy(context.Background(), ProtectionPolicy{TrustedProxies: []string{"104.194.69.137"}})
	proxies, source := registry.TrustedProxies()
	if source != "database" || len(proxies) != 1 || proxies[0] != "104.194.69.137/32" {
		t.Fatalf("got %v from %q, want the stored list to win", proxies, source)
	}
	// Clearing the stored list must fall back rather than strand the deployment.
	registry.SetPolicy(context.Background(), ProtectionPolicy{})
	if _, source = registry.TrustedProxies(); source != "config" {
		t.Errorf("got %q, want the config fallback after clearing the stored list", source)
	}
}
