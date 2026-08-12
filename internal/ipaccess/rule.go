// Package ipaccess implements the operator-managed IP allow/deny list that
// fronts every request, together with the trust check that decides whether the
// observed client address is meaningful at all.
//
// The trust check is not an optional refinement: behind a reverse proxy with no
// trusted-proxies configured, every client reports the proxy's address. Denying
// that address bans everyone and allowing it exempts everyone, so a list that
// ignored trust would be actively dangerous rather than merely useless.
package ipaccess

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Effect is the verdict a rule carries when it matches.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Source records who created a rule. Automatic rules are reclaimable by the
// expiry sweeper; manual rules are never removed without an operator asking.
type Source string

const (
	SourceManual Source = "manual"
	SourceAuto   Source = "auto"
)

// Minimum prefix lengths. A rule coarser than these covers so much of the
// address space that it can only be a mistake: /8 is 16 million addresses, and
// a deny that wide takes the deployment offline while an allow that wide
// silently disables the deny list.
const (
	MinPrefixLenV4 = 8
	MinPrefixLenV6 = 32
)

var (
	ErrEmptyCIDR       = errors.New("ip access: empty address")
	ErrInvalidCIDR     = errors.New("ip access: not a valid IP address or CIDR")
	ErrLoopbackCIDR    = errors.New("ip access: loopback addresses are always permitted and cannot be listed")
	ErrUnspecifiedCIDR = errors.New("ip access: unspecified address cannot be listed")
	ErrInvalidEffect   = errors.New("ip access: effect must be allow or deny")
)

// Rule is one persisted allow/deny entry.
type Rule struct {
	ID        string     `json:"id"`
	CIDR      string     `json:"cidr"`
	Family    int        `json:"family"`
	Effect    Effect     `json:"effect"`
	Source    Source     `json:"source"`
	Reason    string     `json:"reason"`
	Note      string     `json:"note"`
	Enabled   bool       `json:"enabled"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	HitCount  int64      `json:"hit_count"`
	LastHitAt *time.Time `json:"last_hit_at,omitempty"`
}

// Active reports whether a rule should participate in matching at the given
// instant. Expiry is evaluated here rather than only in SQL so a rule that
// lapses between two refresh cycles stops applying immediately.
func (r Rule) Active(now time.Time) bool {
	if !r.Enabled {
		return false
	}
	if r.ExpiresAt != nil && !r.ExpiresAt.After(now) {
		return false
	}
	return true
}

// ParseEffect validates operator input.
func ParseEffect(raw string) (Effect, error) {
	switch Effect(strings.ToLower(strings.TrimSpace(raw))) {
	case EffectAllow:
		return EffectAllow, nil
	case EffectDeny:
		return EffectDeny, nil
	default:
		return "", ErrInvalidEffect
	}
}

// NormalizeCIDR canonicalises operator input into a masked CIDR string and
// reports the address family.
//
// Bare addresses become host routes (/32, /128) so the stored form is always a
// network, and host bits are cleared so "1.2.3.5/24" and "1.2.3.0/24" cannot
// become two rows describing the same network.
func NormalizeCIDR(raw string) (string, int, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", 0, ErrEmptyCIDR
	}
	candidate = strings.TrimPrefix(candidate, "[")
	candidate = strings.TrimSuffix(candidate, "]")
	if zone := strings.IndexByte(candidate, '%'); zone >= 0 {
		candidate = candidate[:zone]
	}

	var network *net.IPNet
	if strings.Contains(candidate, "/") {
		_, parsed, err := net.ParseCIDR(candidate)
		if err != nil || parsed == nil {
			return "", 0, ErrInvalidCIDR
		}
		network = parsed
	} else {
		ip := net.ParseIP(candidate)
		if ip == nil {
			return "", 0, ErrInvalidCIDR
		}
		bits := 128
		if v4 := ip.To4(); v4 != nil {
			ip = v4
			bits = 32
		}
		network = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
	}

	if network.IP.IsLoopback() {
		return "", 0, ErrLoopbackCIDR
	}
	if network.IP.IsUnspecified() {
		return "", 0, ErrUnspecifiedCIDR
	}

	ones, bits := network.Mask.Size()
	if bits == 0 {
		return "", 0, ErrInvalidCIDR
	}
	family := 6
	minPrefix := MinPrefixLenV6
	if bits == 32 {
		family = 4
		minPrefix = MinPrefixLenV4
	}
	if ones < minPrefix {
		return "", 0, fmt.Errorf("ip access: prefix /%d is too broad, minimum is /%d for IPv%d", ones, minPrefix, family)
	}
	return network.String(), family, nil
}

// ParseIPForMatch parses a textual address into a form Matcher.Match accepts,
// tolerating the "host:port" and bracketed shapes that appear in logs and stored
// events. Returns nil when the input names no address.
func ParseIPForMatch(raw string) net.IP {
	normalized, ok := NormalizeObservedIP(raw)
	if !ok {
		return nil
	}
	return net.ParseIP(normalized)
}

// NormalizeObservedIP canonicalises an address seen on a request so it can be
// offered as a pre-filled rule candidate. Unlike NormalizeCIDR it accepts
// "host:port" forms, because that is what RemoteAddr carries.
func NormalizeObservedIP(raw string) (string, bool) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(candidate); err == nil {
		candidate = host
	}
	candidate = strings.TrimPrefix(candidate, "[")
	candidate = strings.TrimSuffix(candidate, "]")
	if zone := strings.IndexByte(candidate, '%'); zone >= 0 {
		candidate = candidate[:zone]
	}
	ip := net.ParseIP(strings.TrimSpace(candidate))
	if ip == nil {
		return "", false
	}
	return ip.String(), true
}
