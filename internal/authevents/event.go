// Package authevents records authentication attempts so an operator can answer
// "who is attacking us" from the panel instead of grepping process logs.
//
// The pre-existing answer was a logrus line at Debug level whose structured
// fields never reached the output, on a deployment that runs with debug off —
// which is to say there was no answer at all.
package authevents

import "time"

// Outcome classifies what happened to an attempt.
const (
	// OutcomeSuccess is a completed authentication. It is recorded because
	// "this source failed ninety times and then succeeded" is the single most
	// important line in a breach timeline, and a failures-only log omits it.
	OutcomeSuccess = "success"
	// OutcomeFailure is a credential that was compared and did not match.
	OutcomeFailure = "failure"
	// OutcomeThrottled is a request rejected by the rate limiter.
	OutcomeThrottled = "throttled"
	// OutcomeBlocked is a request rejected by the IP deny list.
	OutcomeBlocked = "blocked"
	// OutcomeAutoBanned marks the attempt that tipped a source into a ban.
	OutcomeAutoBanned = "auto_banned"
	// OutcomeWouldBan is the observe-mode counterpart of OutcomeAutoBanned: the
	// rule fired but no ban was written.
	OutcomeWouldBan = "would_ban"
)

// Surface names the door the attempt arrived at.
const (
	SurfaceAdminLogin    = "admin_login"
	SurfacePortalLogin   = "portal_login"
	SurfaceManagementKey = "management_key"
	SurfaceRefresh       = "refresh"
	SurfaceRequest       = "request"
)

// Event is one recorded authentication attempt.
type Event struct {
	OccurredAt time.Time `json:"occurred_at"`
	ID         int64     `json:"id,omitempty"`
	IP         string    `json:"ip"`
	// IPPrefix is the throttle bucket key (/32 or /64), so aggregation here
	// matches what the rate limiter counted.
	IPPrefix string `json:"ip_prefix"`
	// Trusted reports whether IP identified a client. Untrusted rows are kept
	// rather than dropped: they are the evidence that trusted-proxies is
	// misconfigured, and hiding them would hide the misconfiguration.
	Trusted     bool   `json:"trusted"`
	Scope       string `json:"scope"`
	Surface     string `json:"surface"`
	Username    string `json:"username"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason"`
	UserAgent   string `json:"user_agent"`
	RequestPath string `json:"request_path"`
	RequestID   string `json:"request_id"`
	TenantID    string `json:"tenant_id,omitempty"`
}

// Field length caps. Attacker-supplied strings must not be able to turn one
// request into an unbounded database row.
const (
	maxUserAgentLen = 200
	maxUsernameLen  = 120
	maxReasonLen    = 200
	maxPathLen      = 200
)

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// sanitized clamps user-controlled fields and fills in defaults.
func (e Event) sanitized() Event {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now()
	}
	e.UserAgent = truncate(e.UserAgent, maxUserAgentLen)
	e.Username = truncate(e.Username, maxUsernameLen)
	e.Reason = truncate(e.Reason, maxReasonLen)
	e.RequestPath = truncate(e.RequestPath, maxPathLen)
	if e.Outcome == "" {
		e.Outcome = OutcomeFailure
	}
	return e
}

// SourceSummary aggregates attempts by source address.
type SourceSummary struct {
	IPPrefix     string     `json:"ip_prefix"`
	SampleIP     string     `json:"sample_ip"`
	Trusted      bool       `json:"trusted"`
	Attempts     int64      `json:"attempts"`
	Failures     int64      `json:"failures"`
	Throttled    int64      `json:"throttled"`
	Blocked      int64      `json:"blocked"`
	Successes    int64      `json:"successes"`
	DistinctUser int64      `json:"distinct_usernames"`
	FirstSeen    time.Time  `json:"first_seen"`
	LastSeen     time.Time  `json:"last_seen"`
	Surfaces     string     `json:"surfaces"`
	RuleEffect   string     `json:"rule_effect,omitempty"`
	RuleID       string     `json:"rule_id,omitempty"`
	RuleExpires  *time.Time `json:"rule_expires_at,omitempty"`
}
