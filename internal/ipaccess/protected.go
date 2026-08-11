package ipaccess

import (
	"net"
	"net/url"
	"sort"
	"strings"
)

// ProtectedReason explains why an address can never be denied.
type ProtectedReason string

const (
	ProtectedLoopback     ProtectedReason = "loopback"
	ProtectedLocalAddress ProtectedReason = "local_address"
	ProtectedTrustedProxy ProtectedReason = "trusted_proxy"
	ProtectedOutboundPeer ProtectedReason = "outbound_proxy"
)

// ProtectedEntry is one un-bannable address or range.
type ProtectedEntry struct {
	CIDR   string          `json:"cidr"`
	Reason ProtectedReason `json:"reason"`
	net    *net.IPNet
}

// protectedSet is the set of addresses that must survive any rule, automatic or
// manual.
//
// It exists because the interesting failure here is self-inflicted: the machine's
// own address is what a reverse proxy, a health probe and any loopback tooling
// present themselves as, so a rule covering it takes the deployment offline
// rather than blocking an attacker. Making these structurally un-bannable is
// strictly better than documenting "please don't ban these" — an operator acting
// under pressure during an attack is exactly who would ban them by accident.
type protectedSet struct {
	entries []ProtectedEntry
}

func (p *protectedSet) match(ip net.IP) (ProtectedEntry, bool) {
	if p == nil || ip == nil {
		return ProtectedEntry{}, false
	}
	for i := range p.entries {
		if p.entries[i].net != nil && p.entries[i].net.Contains(ip) {
			return p.entries[i], true
		}
	}
	return ProtectedEntry{}, false
}

// buildProtectedSet collects every address the process must never reject.
//
// localAddresses are the host's own interface addresses, trustedProxies the
// declared reverse proxies, and outboundProxies the egress proxy pool. The proxy
// pool is included because operators reasonably expect "our proxies" to be
// exempt; in practice an egress proxy never appears as an inbound client, so the
// entry is harmless and prevents a confusing manual rule from being written
// instead.
func buildProtectedSet(localAddresses, trustedProxies, outboundProxies []string) *protectedSet {
	set := &protectedSet{}
	seen := make(map[string]struct{})

	add := func(raw string, reason ProtectedReason) {
		normalized, _, err := NormalizeCIDR(raw)
		if err != nil {
			// Loopback is rejected by NormalizeCIDR (it is already unconditionally
			// admitted), and unparseable input is simply not a protectable address.
			return
		}
		if _, exists := seen[normalized]; exists {
			return
		}
		_, network, parseErr := net.ParseCIDR(normalized)
		if parseErr != nil || network == nil {
			return
		}
		seen[normalized] = struct{}{}
		set.entries = append(set.entries, ProtectedEntry{CIDR: normalized, Reason: reason, net: network})
	}

	for _, address := range localAddresses {
		add(address, ProtectedLocalAddress)
	}
	for _, address := range trustedProxies {
		add(address, ProtectedTrustedProxy)
	}
	for _, address := range outboundProxies {
		add(address, ProtectedOutboundPeer)
	}

	sort.Slice(set.entries, func(i, j int) bool { return set.entries[i].CIDR < set.entries[j].CIDR })
	return set
}

// LocalInterfaceAddresses returns the host's own non-loopback addresses.
//
// Failures are swallowed: an enumeration error must not stop the process from
// starting, and the loopback guard still covers the most important case.
func LocalInterfaceAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() {
			continue
		}
		out = append(out, ipNet.IP.String())
	}
	return out
}

// ProxyHostAddresses extracts the host portion of outbound proxy URLs, keeping
// only entries that are already literal addresses. A hostname is skipped rather
// than resolved: resolution at startup would bake in one answer and silently
// protect the wrong address after a DNS change.
func ProxyHostAddresses(proxyURLs []string) []string {
	out := make([]string, 0, len(proxyURLs))
	for _, raw := range proxyURLs {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Host == "" {
			continue
		}
		host := parsed.Hostname()
		if net.ParseIP(host) == nil {
			continue
		}
		out = append(out, host)
	}
	return out
}
