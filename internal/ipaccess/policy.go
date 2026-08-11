package ipaccess

import (
	"net"
	"strings"
	"time"
)

// AutoBanMode controls what the automatic ban engine does with a source that
// crosses the failure threshold.
type AutoBanMode string

const (
	// AutoBanOff disables evaluation entirely.
	AutoBanOff AutoBanMode = "off"
	// AutoBanObserve records a would_ban event but writes no rule. This is the
	// shipped default: an automatic ban rule that fires on a bad threshold is a
	// self-inflicted outage, so operators get to watch the rule fire against
	// real traffic before it is allowed to act.
	AutoBanObserve AutoBanMode = "observe"
	// AutoBanEnforce writes a temporary deny rule.
	AutoBanEnforce AutoBanMode = "enforce"
)

// AutoBanPolicy is the rule that promotes repeated authentication failures into
// a temporary deny entry.
type AutoBanPolicy struct {
	Mode AutoBanMode `json:"mode"`
	// WindowSeconds is the observation window for counting failures.
	WindowSeconds int `json:"window_seconds"`
	// FailureThreshold is the failure count within the window that triggers a ban.
	// Failures are counted across every authentication surface, because an
	// attacker spreading attempts over login, portal and management-key endpoints
	// is more suspicious than one hammering a single door, not less.
	FailureThreshold int `json:"failure_threshold"`
	// BanMinutes is the first ban duration.
	BanMinutes int `json:"ban_minutes"`
	// MaxBanMinutes caps the doubling escalation applied to repeat offenders.
	MaxBanMinutes int `json:"max_ban_minutes"`
}

// ThrottleOverride mirrors the config.AuthHardening knobs that operators may
// retune at runtime. Zero means "keep the configured or shipped value", so an
// untouched field never silently rewrites config.yaml semantics.
type ThrottleOverride struct {
	LoginFailureWindowSeconds   int `json:"login_failure_window_seconds"`
	AccountFailureLimit         int `json:"account_failure_limit"`
	ManagementKeyFailureLimit   int `json:"management_key_failure_limit"`
	UnauthenticatedRequestLimit int `json:"unauthenticated_request_limit"`
	FailureResetHours           int `json:"failure_reset_hours"`
}

// ProtectionPolicy is the persisted, operator-editable half of the protection
// settings. It is stored as one runtime-settings document rather than a table
// because it is a single object that is read on every refresh and never queried
// by field.
type ProtectionPolicy struct {
	// Lockdown rejects any address that is not explicitly allow-listed.
	Lockdown bool             `json:"lockdown"`
	AutoBan  AutoBanPolicy    `json:"auto_ban"`
	Throttle ThrottleOverride `json:"throttle"`
	// TrustedProxies are the reverse proxies whose forwarding headers may be
	// believed. It lives here, in the database, rather than in config.yaml
	// because it is the single setting that decides whether this whole feature
	// does anything — and an operator watching an attack should be able to fix it
	// from the panel instead of editing a file and restarting the process.
	//
	// config.yaml's trusted-proxies still applies when this is empty, so existing
	// deployments keep working and nothing silently changes under them.
	TrustedProxies []string `json:"trusted_proxies,omitempty"`
	// Alert configures outbound notification for ban decisions.
	Alert AlertPolicy `json:"alert"`
	// AttemptRetentionDays bounds how long authentication attempts are kept.
	// Zero uses the shipped default.
	AttemptRetentionDays int `json:"attempt_retention_days"`
}

// SettingKey is the runtime-settings document key for ProtectionPolicy.
const SettingKey = "ip-access-protection-policy"

// DefaultAttemptRetentionDays is long enough to investigate an incident reported
// a few weeks late, short enough that a sustained attack cannot fill the disk.
const DefaultAttemptRetentionDays = 30

// maxAttemptRetentionDays caps operator input. The attempt table grows with
// attack volume, not with legitimate traffic, so unbounded retention is a
// disk-exhaustion path an attacker controls.
const maxAttemptRetentionDays = 365

const (
	defaultAutoBanWindowSeconds = 600
	defaultAutoBanThreshold     = 20
	defaultAutoBanMinutes       = 60
	defaultAutoBanMaxMinutes    = 24 * 60
)

// DefaultPolicy is the shipped configuration: the list is enforced, but nothing
// bans automatically until an operator has seen the rule fire.
func DefaultPolicy() ProtectionPolicy {
	return ProtectionPolicy{
		Lockdown: false,
		AutoBan: AutoBanPolicy{
			Mode:             AutoBanObserve,
			WindowSeconds:    defaultAutoBanWindowSeconds,
			FailureThreshold: defaultAutoBanThreshold,
			BanMinutes:       defaultAutoBanMinutes,
			MaxBanMinutes:    defaultAutoBanMaxMinutes,
		},
		Alert:                AlertPolicy{CooldownMinutes: defaultAlertCooldownMinutes},
		AttemptRetentionDays: DefaultAttemptRetentionDays,
	}
}

// Normalized clamps operator input into a workable policy. Out-of-range values
// fall back to the default rather than disabling protection, because a zero
// threshold would otherwise ban the first client to mistype a password.
func (p ProtectionPolicy) Normalized() ProtectionPolicy {
	defaults := DefaultPolicy()
	switch p.AutoBan.Mode {
	case AutoBanOff, AutoBanObserve, AutoBanEnforce:
	default:
		p.AutoBan.Mode = defaults.AutoBan.Mode
	}
	if p.AutoBan.WindowSeconds <= 0 {
		p.AutoBan.WindowSeconds = defaults.AutoBan.WindowSeconds
	}
	if p.AutoBan.FailureThreshold <= 0 {
		p.AutoBan.FailureThreshold = defaults.AutoBan.FailureThreshold
	}
	if p.AutoBan.BanMinutes <= 0 {
		p.AutoBan.BanMinutes = defaults.AutoBan.BanMinutes
	}
	if p.AutoBan.MaxBanMinutes <= 0 {
		p.AutoBan.MaxBanMinutes = defaults.AutoBan.MaxBanMinutes
	}
	if p.AutoBan.MaxBanMinutes < p.AutoBan.BanMinutes {
		p.AutoBan.MaxBanMinutes = p.AutoBan.BanMinutes
	}
	if p.Throttle.LoginFailureWindowSeconds < 0 {
		p.Throttle.LoginFailureWindowSeconds = 0
	}
	if p.Throttle.AccountFailureLimit < 0 {
		p.Throttle.AccountFailureLimit = 0
	}
	if p.Throttle.ManagementKeyFailureLimit < 0 {
		p.Throttle.ManagementKeyFailureLimit = 0
	}
	if p.Throttle.UnauthenticatedRequestLimit < 0 {
		p.Throttle.UnauthenticatedRequestLimit = 0
	}
	if p.Throttle.FailureResetHours < 0 {
		p.Throttle.FailureResetHours = 0
	}
	cleaned := make([]string, 0, len(p.TrustedProxies))
	seen := make(map[string]struct{}, len(p.TrustedProxies))
	for _, proxy := range p.TrustedProxies {
		normalized, ok := NormalizeTrustedProxy(proxy)
		if !ok {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		cleaned = append(cleaned, normalized)
	}
	p.TrustedProxies = cleaned
	p.Alert = p.Alert.normalized()
	if p.AttemptRetentionDays <= 0 {
		p.AttemptRetentionDays = DefaultAttemptRetentionDays
	}
	if p.AttemptRetentionDays > maxAttemptRetentionDays {
		p.AttemptRetentionDays = maxAttemptRetentionDays
	}
	return p
}

// NormalizeTrustedProxy canonicalises one entry of the trusted proxy list. A
// bare address becomes a host route so the stored form is always a network.
func NormalizeTrustedProxy(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if strings.Contains(trimmed, "/") {
		_, network, err := net.ParseCIDR(trimmed)
		if err != nil || network == nil {
			return "", false
		}
		return network.String(), true
	}
	ip := net.ParseIP(trimmed)
	if ip == nil {
		return "", false
	}
	bits := 128
	if v4 := ip.To4(); v4 != nil {
		ip, bits = v4, 32
	}
	return (&net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}).String(), true
}

// Window returns the auto-ban observation window.
func (p AutoBanPolicy) Window() time.Duration {
	return time.Duration(p.WindowSeconds) * time.Second
}

// BanDuration returns the ban length for a source that has already been banned
// repeats times, doubling each round up to MaxBanMinutes.
func (p AutoBanPolicy) BanDuration(repeats int) time.Duration {
	minutes := p.BanMinutes
	for i := 0; i < repeats && minutes < p.MaxBanMinutes; i++ {
		minutes *= 2
	}
	if minutes > p.MaxBanMinutes {
		minutes = p.MaxBanMinutes
	}
	return time.Duration(minutes) * time.Minute
}
