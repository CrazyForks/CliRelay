// Package protocol is the frozen wire contract between the CliRelay application
// and the updater sidecar.
//
// Both sides of this contract are replaced independently and in an unpredictable
// order: during an online update the application container is recreated while the
// updater keeps running, and later the updater refreshes itself while the new
// application is already serving. Any given deployment therefore routinely runs an
// old app against a new updater and vice versa.
//
// Consequences, which are load-bearing:
//
//   - Changes here must be additive. Never remove a field, never repurpose one, and
//     never tighten a value that an older peer already emits.
//   - Every wire struct keeps the legacy flat fields alongside newer nested ones so
//     an old peer that only understands the flat form still functions.
//   - compat_test.go pins the serialized shape in both directions. If it fails, the
//     change breaks a live deployment mid-update — fix the change, not the test.
package protocol

import (
	"fmt"
	"strings"
)

const (
	// ProtocolVersion is reported by /v1/health so the application can tell what the
	// sidecar in front of it supports.
	//
	//	1 - polling only (/v1/status)
	//	2 - server-sent status events (/v1/events)
	//	3 - plan-driven updates, Last-Event-ID replay, byte-level pull progress
	ProtocolVersion = 3

	// MinPlanVersion and MaxPlanVersion bound the plan documents this updater accepts.
	// A plan outside the range is rejected rather than guessed at, so an application
	// newer than its sidecar fails loudly instead of half-applying an update.
	MinPlanVersion = 1
	MaxPlanVersion = 1
)

// PrepareCommandAllowlist enumerates the one-shot container commands a plan may ask
// the updater to run from the freshly pulled image.
//
// This allowlist is a security boundary, not a coupling we failed to remove. The
// updater runs with the Docker socket mounted and root privileges; if a plan could
// name an arbitrary command, anyone able to reach the management API could turn the
// sidecar into arbitrary host code execution. The application chooses which
// preparation step runs, but only from commands that are fixed container entrypoints
// with known, bounded behaviour.
var PrepareCommandAllowlist = map[string]struct{}{
	"clirelay-init-env": {},
}

// PrepareEnvPrefix restricts which environment variables a plan may inject into the
// preparation container. Confining them to the project's own namespace keeps a plan
// from rewriting PATH, LD_PRELOAD or other variables that change what actually runs.
const PrepareEnvPrefix = "CLIRELAY_"

// Plan is a declarative description of an update. It carries no knowledge of the
// application's internal structure: the application names the services it wants
// pulled and recreated, and the updater executes that list without parsing or
// interpreting the compose file. Adding a service to the deployment is an
// application-side change and needs no updater release.
type Plan struct {
	PlanVersion int `json:"plan_version"`

	// Image and Tag identify the target build. The updater refuses a repository that
	// differs from the one already configured in the environment file, so a plan can
	// move the deployment forward on its own image but never onto a foreign one.
	Image string `json:"image"`
	Tag   string `json:"tag"`

	// Prepare runs before anything is recreated, as a one-shot container built from
	// the newly pulled image. This is where deployment topology and environment
	// migration lives: the new build knows what it needs, so it migrates itself.
	// The updater neither reads nor writes the compose file.
	Prepare *PrepareStep `json:"prepare,omitempty"`

	// Pull lists the services whose images should be fetched before recreation.
	Pull []string `json:"pull,omitempty"`

	// Stages are executed in order. Each stage recreates a set of services and
	// optionally waits for them to report healthy.
	Stages []Stage `json:"stages,omitempty"`

	// SelfRefreshService, when set, names the updater's own compose service. After a
	// successful update the updater schedules a detached helper to recreate it so the
	// sidecar also lands on the new image. Empty disables self-refresh.
	SelfRefreshService string `json:"self_refresh_service,omitempty"`
}

// PrepareStep describes the one-shot preparation container.
//
// Mounts are intentionally absent: the updater derives the project directory mount
// from its own configuration rather than accepting one from the caller. A
// caller-supplied mount would let a plan bind the host root into a privileged
// container.
type PrepareStep struct {
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// Optional lets the update continue when preparation fails. Used for steps that
	// are best-effort on deployments that predate them.
	Optional bool `json:"optional,omitempty"`
}

// Stage is one recreation step.
type Stage struct {
	// ID is a stable identifier used for progress reporting and translation lookup.
	ID string `json:"id"`

	Services []string `json:"services"`

	// WaitHealthy passes --wait so compose blocks until healthchecks pass.
	WaitHealthy bool `json:"wait_healthy,omitempty"`

	// NoDeps avoids compose pulling dependencies into the recreation set, which would
	// otherwise restart the database underneath a running application.
	NoDeps        bool `json:"no_deps,omitempty"`
	RemoveOrphans bool `json:"remove_orphans,omitempty"`

	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// Optional marks a stage that may be skipped when none of its services exist in
	// the deployment, so a plan written for the current topology still applies to an
	// older compose file.
	Optional bool `json:"optional,omitempty"`

	// Weight is this stage's share of the overall progress bar, relative to the sum
	// of all weights. Defaults to 1 when unset.
	Weight int `json:"weight,omitempty"`
}

// UpdateRequest is the POST /v1/update body.
//
// The flat fields are the protocol 2 shape and are still populated by current
// applications: an updater that predates plans reads them and performs a default
// single-service update, which is why an application can be upgraded before its
// sidecar is. Plan-aware updaters prefer Plan and use the flat fields only as
// display metadata.
type UpdateRequest struct {
	Plan *Plan `json:"plan,omitempty"`

	Service string `json:"service"`
	Image   string `json:"image"`
	Tag     string `json:"tag"`

	CurrentVersion   string `json:"current_version"`
	CurrentCommit    string `json:"current_commit"`
	CurrentUIVersion string `json:"current_ui_version"`
	CurrentUICommit  string `json:"current_ui_commit"`

	Version     string `json:"version"`
	Commit      string `json:"commit"`
	CommitURL   string `json:"commit_url"`
	UIVersion   string `json:"ui_version"`
	UICommit    string `json:"ui_commit"`
	UICommitURL string `json:"ui_commit_url"`

	Channel string `json:"channel"`

	ReleaseName        string `json:"release_name"`
	ReleaseTag         string `json:"release_tag"`
	ReleaseNotes       string `json:"release_notes"`
	ReleaseURL         string `json:"release_url"`
	ReleasePublishedAt string `json:"release_published_at"`
}

// UpdateAccepted is the 202 response body for POST /v1/update.
type UpdateAccepted struct {
	Status  string `json:"status"`
	Service string `json:"service,omitempty"`
	RunID   uint64 `json:"run_id"`
}

// Health is the GET /v1/health response body.
type Health struct {
	Status          string `json:"status"`
	Error           string `json:"error"`
	ProtocolVersion int    `json:"protocol_version"`
	Events          string `json:"events"`

	// MaxPlanVersion lets the application decide whether to send a plan at all,
	// instead of sending one and discovering mid-update that it was ignored.
	MaxPlanVersion int `json:"max_plan_version,omitempty"`
}

// SanitizeServiceName returns the service name if it is safe to hand to the docker
// CLI as an argument, or "" if it is not. Service names reach an exec argv, so
// anything outside this character set is rejected rather than escaped.
func SanitizeServiceName(service string) string {
	trimmed := strings.TrimSpace(service)
	if trimmed == "" {
		return ""
	}
	for _, r := range trimmed {
		switch {
		case r == '-' || r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		default:
			return ""
		}
	}
	return trimmed
}

// IsSafeImagePart reports whether an image or tag fragment can be embedded in the
// environment file and passed to docker without quoting hazards.
func IsSafeImagePart(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r <= ' ' || r == '\'' || r == '"' || r == '\\' || r == '`' || r == '$' {
			return false
		}
	}
	return true
}

// ImageRef joins an image and tag into a reference, or returns "" when either part
// is missing or unsafe.
func ImageRef(image string, tag string) string {
	cleanImage := strings.TrimSpace(image)
	cleanTag := strings.TrimSpace(tag)
	if cleanImage == "" || cleanTag == "" {
		return ""
	}
	if !IsSafeImagePart(cleanImage) || !IsSafeImagePart(cleanTag) {
		return ""
	}
	return cleanImage + ":" + cleanTag
}

// ImageRepository strips the tag or digest from an image reference.
func ImageRepository(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if digestIndex := strings.Index(ref, "@"); digestIndex >= 0 {
		return ref[:digestIndex]
	}
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon]
	}
	return ref
}

// Validate checks a plan against the security and compatibility rules above and
// returns a normalized copy. It never mutates the receiver.
//
// Validation is deliberately strict: this input arrives from the network and drives
// a process holding the Docker socket.
func (p *Plan) Validate() (*Plan, error) {
	if p == nil {
		return nil, fmt.Errorf("plan is missing")
	}
	if p.PlanVersion < MinPlanVersion || p.PlanVersion > MaxPlanVersion {
		return nil, fmt.Errorf(
			"unsupported plan_version %d: this updater supports %d-%d; recreate the clirelay-updater service to upgrade it",
			p.PlanVersion, MinPlanVersion, MaxPlanVersion,
		)
	}
	if ImageRef(p.Image, p.Tag) == "" {
		return nil, fmt.Errorf("plan image or tag is missing or unsafe")
	}

	normalized := &Plan{
		PlanVersion:        p.PlanVersion,
		Image:              strings.TrimSpace(p.Image),
		Tag:                strings.TrimSpace(p.Tag),
		SelfRefreshService: SanitizeServiceName(p.SelfRefreshService),
	}

	if p.Prepare != nil {
		prepare, err := p.Prepare.validate()
		if err != nil {
			return nil, err
		}
		normalized.Prepare = prepare
	}

	for _, service := range p.Pull {
		clean := SanitizeServiceName(service)
		if clean == "" {
			return nil, fmt.Errorf("plan pull list contains an invalid service name %q", service)
		}
		normalized.Pull = append(normalized.Pull, clean)
	}

	if len(p.Stages) == 0 {
		return nil, fmt.Errorf("plan has no stages")
	}
	for index, stage := range p.Stages {
		clean, err := stage.validate()
		if err != nil {
			return nil, fmt.Errorf("plan stage %d: %w", index, err)
		}
		normalized.Stages = append(normalized.Stages, clean)
	}
	return normalized, nil
}

func (s *PrepareStep) validate() (*PrepareStep, error) {
	if len(s.Command) == 0 {
		return nil, fmt.Errorf("prepare step has no command")
	}
	entrypoint := strings.TrimSpace(s.Command[0])
	if _, ok := PrepareCommandAllowlist[entrypoint]; !ok {
		return nil, fmt.Errorf(
			"prepare command %q is not allowed; the updater only runs known container entrypoints",
			entrypoint,
		)
	}
	clean := &PrepareStep{Optional: s.Optional, Command: []string{entrypoint}}
	// Arguments are rejected rather than escaped: the allowlisted entrypoints take
	// their input from the environment, so an argument here would only ever be an
	// attempt to reach something else.
	if len(s.Command) > 1 {
		return nil, fmt.Errorf("prepare command %q does not accept arguments", entrypoint)
	}
	if len(s.Env) > 0 {
		clean.Env = make(map[string]string, len(s.Env))
		for key, value := range s.Env {
			trimmedKey := strings.TrimSpace(key)
			if !strings.HasPrefix(trimmedKey, PrepareEnvPrefix) {
				return nil, fmt.Errorf(
					"prepare environment key %q is not allowed; only %s* may be set",
					key, PrepareEnvPrefix,
				)
			}
			if strings.ContainsAny(value, "\x00\n\r") {
				return nil, fmt.Errorf("prepare environment value for %q contains a control character", key)
			}
			clean.Env[trimmedKey] = value
		}
	}
	return clean, nil
}

func (s Stage) validate() (Stage, error) {
	clean := Stage{
		ID:             strings.TrimSpace(s.ID),
		WaitHealthy:    s.WaitHealthy,
		NoDeps:         s.NoDeps,
		RemoveOrphans:  s.RemoveOrphans,
		TimeoutSeconds: s.TimeoutSeconds,
		Optional:       s.Optional,
		Weight:         s.Weight,
	}
	if clean.ID == "" {
		return clean, fmt.Errorf("stage id is empty")
	}
	if SanitizeServiceName(clean.ID) == "" {
		return clean, fmt.Errorf("stage id %q is invalid", s.ID)
	}
	if len(s.Services) == 0 {
		return clean, fmt.Errorf("stage %q has no services", clean.ID)
	}
	for _, service := range s.Services {
		name := SanitizeServiceName(service)
		if name == "" {
			return clean, fmt.Errorf("stage %q contains an invalid service name %q", clean.ID, service)
		}
		clean.Services = append(clean.Services, name)
	}
	if clean.TimeoutSeconds < 0 {
		return clean, fmt.Errorf("stage %q has a negative timeout", clean.ID)
	}
	if clean.Weight <= 0 {
		clean.Weight = 1
	}
	return clean, nil
}

// TotalWeight is the denominator for stage progress.
func (p *Plan) TotalWeight() int {
	total := 0
	for _, stage := range p.Stages {
		weight := stage.Weight
		if weight <= 0 {
			weight = 1
		}
		total += weight
	}
	if total <= 0 {
		return 1
	}
	return total
}
