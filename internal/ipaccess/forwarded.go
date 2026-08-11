package ipaccess

import (
	"net"
	"net/http"
	"strings"
)

// forwardedHeaders are walked in priority order. X-Forwarded-For is the de-facto
// standard; X-Real-IP carries a single address; Forwarded is RFC 7239.
var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"}

// trustedProxyMatcher decides whether a hop may be believed.
type trustedProxyMatcher struct {
	nets []*net.IPNet
}

func newTrustedProxyMatcher(cidrs []string) *trustedProxyMatcher {
	matcher := &trustedProxyMatcher{}
	for _, raw := range cidrs {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if !strings.Contains(trimmed, "/") {
			if ip := net.ParseIP(trimmed); ip != nil {
				bits := 128
				if v4 := ip.To4(); v4 != nil {
					ip, bits = v4, 32
				}
				matcher.nets = append(matcher.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			}
			continue
		}
		if _, network, err := net.ParseCIDR(trimmed); err == nil && network != nil {
			matcher.nets = append(matcher.nets, network)
		}
	}
	return matcher
}

func (m *trustedProxyMatcher) configured() bool { return m != nil && len(m.nets) > 0 }

func (m *trustedProxyMatcher) trusts(ip net.IP) bool {
	if m == nil || ip == nil {
		return false
	}
	for _, network := range m.nets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveForwarded walks the forwarding chain from the direct peer outwards and
// returns the first address that no trusted proxy vouched for — that is the
// client.
//
// The walk is right-to-left because only the rightmost entries were appended by
// infrastructure we control; everything to the left of the first untrusted hop
// is attacker-supplied and must be discarded, not merely deprioritised. A single
// spoofed X-Forwarded-For would otherwise let a banned client rename itself, or
// forge membership of the allow list.
//
// Returns trusted=false when the chain cannot be resolved to a real client:
// either nothing is configured as a proxy while the request was clearly relayed,
// or every hop is a trusted proxy and the real client is simply not in the
// header. Both cases mean the address identifies infrastructure rather than a
// client, and the caller must not enforce anything on it.
func resolveForwarded(req *http.Request, matcher *trustedProxyMatcher) ClientAddress {
	addr := ClientAddress{}
	if req == nil {
		return addr
	}
	addr.RelayHeader = firstForwardedHeader(req)
	addr.Chain = collectForwardedChain(req)
	addr.DirectPeer = addr.RelayHeader == ""

	peerText := hostOnly(req.RemoteAddr)
	peer := net.ParseIP(peerText)
	addr.Peer, addr.Raw, addr.IP = peerText, peerText, peer

	if peer == nil {
		return addr
	}
	// A direct connection from something that is not a declared proxy is already
	// the client, headers or not: whatever it claims about upstream hops is
	// unverifiable, so it is ignored.
	if !matcher.trusts(peer) {
		addr.Trusted = true
		if addr.RelayHeader != "" && !matcher.configured() {
			// Relayed but no proxy declared: this is the proxy's address standing
			// in for every client behind it.
			addr.Trusted = false
		}
		return addr
	}

	// The peer is a declared proxy, so its forwarding header may be believed as
	// far as the first hop it did not add itself.
	for index := len(forwardedHeaders) - 1; index >= 0; index-- {
		hops := parseForwardedHeader(req, forwardedHeaders[index])
		for i := len(hops) - 1; i >= 0; i-- {
			ip := net.ParseIP(hops[i])
			if ip == nil {
				continue
			}
			if matcher.trusts(ip) {
				continue
			}
			resolved := ClientAddress{
				Peer:        addr.Peer,
				Chain:       addr.Chain,
				IP:          ip,
				Raw:         ip.String(),
				Trusted:     true,
				RelayHeader: addr.RelayHeader,
			}
			// A relayed chain that dead-ends on a loopback address has not been
			// resolved: some hop in front of us is talking to the next over
			// loopback and was never declared. Trusting it would hand every
			// external client the local-operator exemption, which is exactly the
			// "everyone is localhost" failure this guard exists to prevent.
			if ip.IsLoopback() {
				resolved.Trusted = false
			}
			return resolved
		}
	}
	// Every hop was a trusted proxy: the real client never made it into the
	// header, so nothing here identifies one.
	return addr
}

// collectForwardedChain returns every hop seen across the forwarding headers,
// left to right, purely for diagnostics. Operators cannot declare the right
// proxies without being able to see what the chain actually contains.
func collectForwardedChain(req *http.Request) []string {
	var chain []string
	for _, header := range forwardedHeaders {
		chain = append(chain, parseForwardedHeader(req, header)...)
	}
	return chain
}

func firstForwardedHeader(req *http.Request) string {
	for _, header := range forwardedHeaders {
		for _, value := range req.Header.Values(header) {
			if strings.TrimSpace(value) != "" {
				return header
			}
		}
	}
	return ""
}

func parseForwardedHeader(req *http.Request, header string) []string {
	values := req.Header.Values(header)
	if len(values) == 0 {
		return nil
	}
	hops := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			candidate := strings.TrimSpace(part)
			if candidate == "" {
				continue
			}
			// RFC 7239 shape: for=192.0.2.1;proto=https
			if header == "Forwarded" {
				candidate = extractForwardedFor(candidate)
				if candidate == "" {
					continue
				}
			}
			hops = append(hops, hostOnly(candidate))
		}
	}
	return hops
}

func extractForwardedFor(directive string) string {
	for _, token := range strings.Split(directive, ";") {
		token = strings.TrimSpace(token)
		if !strings.HasPrefix(strings.ToLower(token), "for=") {
			continue
		}
		value := strings.TrimPrefix(token[4:], "\"")
		return strings.TrimSuffix(value, "\"")
	}
	return ""
}

// hostOnly strips a port and IPv6 brackets, leaving a bare address.
func hostOnly(raw string) string {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(candidate); err == nil {
		candidate = host
	}
	candidate = strings.TrimPrefix(candidate, "[")
	candidate = strings.TrimSuffix(candidate, "]")
	if zone := strings.IndexByte(candidate, '%'); zone >= 0 {
		candidate = candidate[:zone]
	}
	return strings.TrimSpace(candidate)
}
