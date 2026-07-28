package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"clirelay.local/updater/protocol"
)

// Options configure a single plan execution.
type Options struct {
	Docker      Docker
	ProjectDir  string
	SelfRunID   uint64
	SkipPrepare bool
}

// Execute runs a validated plan.
//
// The whole method is deliberately a flat sequence of declared steps. It performs no
// inspection of what the services are, what they run, or how they relate to one
// another — that knowledge lives in the application that authored the plan. A new
// service, a renamed container or a reordered dependency graph is a plan change, not
// an updater change.
func Execute(ctx context.Context, plan *protocol.Plan, options Options, reporter Reporter) error {
	stages := newStageTracker(plan)

	if plan.Prepare != nil && !options.SkipPrepare {
		stages.begin(reporter, protocol.StagePreparing, "preparing_deployment", "preparing deployment configuration")
		if err := runPrepare(ctx, plan, options, reporter); err != nil {
			if !plan.Prepare.Optional {
				return fmt.Errorf("prepare deployment: %w", err)
			}
			reporter.Log("stderr", "optional preparation step failed, continuing: "+err.Error())
		}
	}

	if len(plan.Pull) > 0 {
		if err := runPull(ctx, plan, options, reporter, stages); err != nil {
			return err
		}
	}

	// Ask compose once which services exist so optional stages can be skipped on
	// deployments whose compose file predates them.
	available, err := options.Docker.ServiceNames(ctx)
	if err != nil {
		// Not fatal: if compose cannot enumerate services, run every stage and let
		// compose itself reject anything genuinely missing.
		reporter.Log("stderr", "could not enumerate compose services: "+err.Error())
		available = nil
	}

	for _, stage := range plan.Stages {
		services := presentServices(stage.Services, available)
		if len(services) == 0 {
			if stage.Optional {
				stages.skip(reporter, stage.ID)
				reporter.Log("stdout", "skipping stage "+stage.ID+": none of its services exist in this deployment")
				continue
			}
			return fmt.Errorf("stage %q requires services %s, which this deployment does not define",
				stage.ID, strings.Join(stage.Services, ", "))
		}

		stages.beginStage(reporter, stage.ID)
		args := []string{"up", "-d"}
		if stage.NoDeps {
			args = append(args, "--no-deps")
		}
		if stage.RemoveOrphans {
			args = append(args, "--remove-orphans")
		}
		if stage.WaitHealthy {
			args = append(args, "--wait")
			if stage.TimeoutSeconds > 0 {
				args = append(args, "--wait-timeout", strconv.Itoa(stage.TimeoutSeconds))
			}
		}
		args = append(args, services...)

		if err := options.Docker.RunCompose(ctx, reporter, nil, args...); err != nil {
			stages.fail(reporter, stage.ID)
			return err
		}
		stages.completeStage(reporter, stage.ID)
	}

	stages.finish(reporter)
	return nil
}

func runPrepare(ctx context.Context, plan *protocol.Plan, options Options, reporter Reporter) error {
	imageRef := protocol.ImageRef(plan.Image, plan.Tag)
	if imageRef == "" {
		return fmt.Errorf("prepare requires a valid target image")
	}
	if strings.TrimSpace(options.ProjectDir) == "" {
		return fmt.Errorf("prepare requires a resolvable project directory")
	}

	args := []string{"run", "--rm"}
	// The mount is derived here rather than taken from the plan; see PrepareStep.
	args = append(args, "-v", options.ProjectDir+":/clirelay-deploy")
	args = append(args, "-e", "CLIRELAY_ENV_FILE=/clirelay-deploy/.env")
	args = append(args, "-e", "CLIRELAY_PROJECT_DIR="+options.ProjectDir)
	for key, value := range plan.Prepare.Env {
		args = append(args, "-e", key+"="+value)
	}
	args = append(args, imageRef)
	args = append(args, plan.Prepare.Command...)

	return RunOneShot(ctx, reporter, args...)
}

func runPull(ctx context.Context, plan *protocol.Plan, options Options, reporter Reporter, stages *stageTracker) error {
	tracker := NewPullTracker()
	stages.begin(reporter, protocol.StagePulling, "pulling_target_image", "pulling the target image")

	onLine := func(line string) {
		tracker.Observe(line)
		snapshot := tracker.Snapshot()
		if snapshot.Percent < 0 {
			return
		}
		reporter.Progress(ProgressUpdate{
			Stage:       protocol.StagePulling,
			MessageCode: "pulling_target_image",
			Message:     "pulling the target image",
			Percent:     stages.percentWithin(protocol.StagePulling, snapshot.Percent),
			Bytes:       snapshot.Bytes,
			TotalBytes:  snapshot.TotalBytes,
			Stages:      stages.states(),
		})
	}

	args := append([]string{"pull"}, plan.Pull...)
	if err := options.Docker.RunCompose(ctx, reporter, onLine, args...); err != nil {
		return err
	}

	// Compose exits 0 when it decides not to pull. Treating that as success would
	// leave the deployment on the old image while reporting a completed update, so
	// it is surfaced as a failure with the reason compose gave.
	if message, skipped := tracker.Skipped(); skipped {
		detail := "docker compose pull skipped the target service; check the pull policy and image refresh settings"
		if strings.TrimSpace(message) != "" {
			detail += ": " + strings.TrimSpace(message)
		}
		return fmt.Errorf("%s", detail)
	}
	stages.complete(reporter, protocol.StagePulling)
	return nil
}

// presentServices filters a stage's services down to those the deployment defines.
// A nil availability set means "unknown", in which case every service is attempted.
func presentServices(services []string, available map[string]struct{}) []string {
	if available == nil {
		return services
	}
	present := make([]string, 0, len(services))
	for _, service := range services {
		if _, ok := available[service]; ok {
			present = append(present, service)
		}
	}
	return present
}

// ScheduleSelfRefresh starts a detached helper from the newly pulled image that
// recreates the updater's own compose service.
//
// The helper has to be a separate container: the updater cannot recreate itself,
// because compose would kill this process partway through the operation. The helper
// waits briefly so this process can finish reporting a completed status and persist
// it, which is what the replacement process restores and republishes on startup.
func ScheduleSelfRefresh(ctx context.Context, plan *protocol.Plan, options Options, runID uint64, reporter Reporter) (bool, error) {
	service := strings.TrimSpace(plan.SelfRefreshService)
	if service == "" {
		return false, nil
	}
	available, err := options.Docker.ServiceNames(ctx)
	if err == nil && available != nil {
		if _, ok := available[service]; !ok {
			return false, nil
		}
	}

	imageRef := protocol.ImageRef(plan.Image, plan.Tag)
	if imageRef == "" {
		return false, fmt.Errorf("cannot refresh the updater sidecar without a target image")
	}

	composeArgs := options.Docker.ComposeArgs("up", "-d", "--no-deps", "--force-recreate", service)
	quoted := make([]string, 0, len(composeArgs))
	for _, arg := range composeArgs {
		quoted = append(quoted, shellQuote(arg))
	}

	reporter.Stage(protocol.StageFinalizing, "scheduling the updater sidecar refresh")
	id, err := RunDetached(ctx,
		"run", "--rm", "-d",
		"--name", "clirelay-updater-refresh-"+strconv.FormatUint(runID, 10),
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-v", options.ProjectDir+":"+options.ProjectDir,
		"-w", options.ProjectDir,
		imageRef,
		"sh", "-c", "sleep 2; exec docker "+strings.Join(quoted, " "),
	)
	if err != nil {
		return false, fmt.Errorf("schedule the updater sidecar refresh: %w", err)
	}
	reporter.Log("stdout", "scheduled the updater sidecar refresh helper "+id)
	return true, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
