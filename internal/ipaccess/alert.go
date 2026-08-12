package ipaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// AlertPolicy configures outbound notification for ban decisions.
//
// Without a channel the feature only writes rows an operator has to go and look
// at, which is no help at 03:00 — the case the automatic ban exists for.
type AlertPolicy struct {
	// WebhookURL receives a JSON POST per alert. Empty disables alerting.
	WebhookURL string `json:"webhook_url,omitempty"`
	// NotifyObserve also sends alerts while auto-ban is in observe mode, so a
	// threshold can be validated against real traffic before it is allowed to act.
	NotifyObserve bool `json:"notify_observe"`
	// CooldownMinutes suppresses repeats for the same source. Under attack the
	// engine can decide about one source many times, and an alert channel that
	// relays each one is a channel operators mute — which is worse than none.
	CooldownMinutes int `json:"cooldown_minutes"`
}

const (
	defaultAlertCooldownMinutes = 30
	// alertTimeout bounds one delivery. Alerting must never become the slowest
	// part of rejecting a request.
	alertTimeout = 5 * time.Second
	// maxTrackedAlertKeys bounds the dedup table; an attacker minting sources
	// must not be able to grow it without limit.
	maxTrackedAlertKeys = 4096
)

func (p AlertPolicy) normalized() AlertPolicy {
	p.WebhookURL = strings.TrimSpace(p.WebhookURL)
	if p.CooldownMinutes <= 0 {
		p.CooldownMinutes = defaultAlertCooldownMinutes
	}
	return p
}

// Enabled reports whether alerts should be delivered at all.
func (p AlertPolicy) Enabled() bool { return strings.TrimSpace(p.WebhookURL) != "" }

// AlertEvent is the payload delivered to the webhook.
type AlertEvent struct {
	Event      string    `json:"event"`
	CIDR       string    `json:"cidr"`
	Failures   int       `json:"failures"`
	Window     string    `json:"window"`
	Reason     string    `json:"reason"`
	Enforced   bool      `json:"enforced"`
	ExpiresAt  string    `json:"expires_at,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// alerter delivers ban notifications with per-source cooldown.
type alerter struct {
	client *http.Client

	mu   sync.Mutex
	last map[string]time.Time
}

func newAlerter() *alerter {
	return &alerter{
		client: &http.Client{Timeout: alertTimeout},
		last:   make(map[string]time.Time),
	}
}

// shouldSend reports whether this source is outside its cooldown, and records
// the send. Returns false when suppressed.
func (a *alerter) shouldSend(key string, cooldown time.Duration, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if previous, ok := a.last[key]; ok && now.Sub(previous) < cooldown {
		return false
	}
	if len(a.last) >= maxTrackedAlertKeys {
		// Drop everything already outside its cooldown; entries still inside it
		// are the ones actively suppressing noise and must be kept.
		for existing, at := range a.last {
			if now.Sub(at) >= cooldown {
				delete(a.last, existing)
			}
		}
		if len(a.last) >= maxTrackedAlertKeys {
			return false
		}
	}
	a.last[key] = now
	return true
}

// deliver posts the event. Failures are logged and swallowed: an unreachable
// webhook must never turn into a failed ban or a slow request.
func (a *alerter) deliver(ctx context.Context, url string, event AlertEvent) {
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		log.WithError(err).Warn("ip-access: alert webhook URL is not usable")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		log.WithError(err).Warn("ip-access: alert delivery failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		log.Warnf("ip-access: alert webhook returned %d", resp.StatusCode)
	}
}

// notifyBan sends an alert for a ban decision, honouring mode and cooldown.
func (r *Registry) notifyBan(outcome AutoBanOutcome, reason string) {
	if r == nil || !outcome.Triggered {
		return
	}
	policy := r.Policy()
	alert := policy.Alert.normalized()
	if !alert.Enabled() {
		return
	}
	// In observe mode nothing was blocked, so alerting is opt-in: an operator
	// validating a threshold wants to see it, one who already trusts it does not.
	if !outcome.Enforced && !alert.NotifyObserve {
		return
	}
	now := time.Now()
	if !r.alerter.shouldSend(outcome.CIDR, time.Duration(alert.CooldownMinutes)*time.Minute, now) {
		return
	}

	event := AlertEvent{
		Event:      "ip_access.auto_ban",
		CIDR:       outcome.CIDR,
		Failures:   outcome.Failures,
		Window:     policy.AutoBan.Window().String(),
		Reason:     reason,
		Enforced:   outcome.Enforced,
		OccurredAt: now,
	}
	if !outcome.Until.IsZero() {
		event.ExpiresAt = outcome.Until.Format(time.RFC3339)
	}
	if !outcome.Enforced {
		event.Event = "ip_access.auto_ban_observed"
	}
	// Delivery is detached from the request that triggered it: the client being
	// rejected is often the one that disconnects, and an alert must not die with it.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), alertTimeout)
		defer cancel()
		r.alerter.deliver(ctx, alert.WebhookURL, event)
	}()
}
