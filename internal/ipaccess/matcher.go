package ipaccess

import (
	"net"
	"sort"
	"time"
)

// Decision is the outcome of matching an address against the list.
type Decision uint8

const (
	// DecisionNeutral means no rule matched. Under lockdown the caller turns
	// this into a rejection; otherwise it is a pass.
	DecisionNeutral Decision = iota
	DecisionAllow
	DecisionDeny
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionDeny:
		return "deny"
	default:
		return "neutral"
	}
}

// prefixIndex stores rules bucketed by prefix length, so a lookup costs one map
// probe per distinct prefix length present rather than a scan over every rule.
// Operators typically use a handful of prefix lengths, so this stays at a few
// probes even with thousands of rules on the request hot path.
type prefixIndex struct {
	v4     map[int]map[[4]byte]*Rule
	v6     map[int]map[[16]byte]*Rule
	v4Lens []int
	v6Lens []int
}

func newPrefixIndex() *prefixIndex {
	return &prefixIndex{
		v4: make(map[int]map[[4]byte]*Rule),
		v6: make(map[int]map[[16]byte]*Rule),
	}
}

func (p *prefixIndex) add(rule *Rule, network *net.IPNet) {
	ones, bits := network.Mask.Size()
	if bits == 32 {
		var key [4]byte
		copy(key[:], network.IP.To4())
		bucket, ok := p.v4[ones]
		if !ok {
			bucket = make(map[[4]byte]*Rule)
			p.v4[ones] = bucket
			p.v4Lens = append(p.v4Lens, ones)
		}
		bucket[key] = rule
		return
	}
	var key [16]byte
	copy(key[:], network.IP.To16())
	bucket, ok := p.v6[ones]
	if !ok {
		bucket = make(map[[16]byte]*Rule)
		p.v6[ones] = bucket
		p.v6Lens = append(p.v6Lens, ones)
	}
	bucket[key] = rule
}

// seal orders prefix lengths from most to least specific so lookup can return
// the longest-prefix match on its first hit.
func (p *prefixIndex) seal() {
	sort.Sort(sort.Reverse(sort.IntSlice(p.v4Lens)))
	sort.Sort(sort.Reverse(sort.IntSlice(p.v6Lens)))
}

func (p *prefixIndex) lookup(ip net.IP) *Rule {
	if v4 := ip.To4(); v4 != nil {
		for _, ones := range p.v4Lens {
			bucket := p.v4[ones]
			if len(bucket) == 0 {
				continue
			}
			var key [4]byte
			mask := net.CIDRMask(ones, 32)
			for i := 0; i < 4; i++ {
				key[i] = v4[i] & mask[i]
			}
			if rule, ok := bucket[key]; ok {
				return rule
			}
		}
		return nil
	}
	v6 := ip.To16()
	if v6 == nil {
		return nil
	}
	for _, ones := range p.v6Lens {
		bucket := p.v6[ones]
		if len(bucket) == 0 {
			continue
		}
		var key [16]byte
		mask := net.CIDRMask(ones, 128)
		for i := 0; i < 16; i++ {
			key[i] = v6[i] & mask[i]
		}
		if rule, ok := bucket[key]; ok {
			return rule
		}
	}
	return nil
}

// Matcher is an immutable snapshot of the active rule set. It is rebuilt on
// refresh and swapped in atomically, so a request never observes a half-applied
// rule change.
type Matcher struct {
	allow    *prefixIndex
	deny     *prefixIndex
	lockdown bool
	count    int
}

// NewMatcher builds a snapshot from the rules that are active at now.
//
// lockdown turns the list into an admission list: anything that does not match
// an allow rule is rejected. It is stored on the snapshot rather than read from
// config at match time so that "which rules were live" and "was lockdown on"
// can never disagree for a single request.
func NewMatcher(rules []Rule, lockdown bool, now time.Time) *Matcher {
	m := &Matcher{allow: newPrefixIndex(), deny: newPrefixIndex(), lockdown: lockdown}
	for i := range rules {
		rule := rules[i]
		if !rule.Active(now) {
			continue
		}
		_, network, err := net.ParseCIDR(rule.CIDR)
		if err != nil || network == nil {
			// Stored rows are normalised on write, so this only fires for data
			// edited outside the API. Skipping beats failing the whole snapshot
			// and dropping every other rule with it.
			continue
		}
		stored := rule
		switch rule.Effect {
		case EffectAllow:
			m.allow.add(&stored, network)
		case EffectDeny:
			m.deny.add(&stored, network)
		default:
			continue
		}
		m.count++
	}
	m.allow.seal()
	m.deny.seal()
	return m
}

// Match resolves an address against the snapshot.
//
// Allow wins over deny by design: a specific exception must be able to override
// a broad ban, otherwise banning 1.2.0.0/16 permanently locks out the one office
// machine inside it and the only fix is deleting the ban entirely.
func (m *Matcher) Match(ip net.IP) (Decision, *Rule) {
	if m == nil || ip == nil {
		return DecisionNeutral, nil
	}
	if rule := m.allow.lookup(ip); rule != nil {
		return DecisionAllow, rule
	}
	if rule := m.deny.lookup(ip); rule != nil {
		return DecisionDeny, rule
	}
	if m.lockdown {
		return DecisionDeny, nil
	}
	return DecisionNeutral, nil
}

// Lockdown reports whether this snapshot rejects unlisted addresses.
func (m *Matcher) Lockdown() bool {
	return m != nil && m.lockdown
}

// Count reports how many rules are live in this snapshot.
func (m *Matcher) Count() int {
	if m == nil {
		return 0
	}
	return m.count
}

// AllowsAddress reports whether an address is explicitly allow-listed. It backs
// the self-lockout guard: enabling lockdown is only safe if the operator making
// the request would still be admitted afterwards.
func (m *Matcher) AllowsAddress(ip net.IP) bool {
	if m == nil || ip == nil {
		return false
	}
	return m.allow.lookup(ip) != nil
}
