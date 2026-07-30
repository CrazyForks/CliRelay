package updateflow

import (
	"os"
	"strings"

	"clirelay.local/updater/protocol"
)

// Deployment service names. These belong here, in the application, because the
// application is what knows its own deployment shape. The updater receives them as
// data and never learns what any of them are for — which is why adding a service
// here is a one-line change rather than an updater release.
const (
	dependencyStageID  = "dependencies"
	applicationStageID = "application"

	defaultPostgresService = "postgres"
	defaultRedisService    = "redis"
	defaultUpdaterService  = "clirelay-updater"

	// applicationWaitTimeoutSeconds bounds how long compose waits for the
	// application to report healthy. Startup does migrations and backfills before it
	// listens, which on a small host has been observed to take the better part of a
	// minute, so this is generous on purpose.
	applicationWaitTimeoutSeconds = 180
)

// BuildUpdatePlan describes the update in terms the updater can execute without
// understanding it.
//
// The plan is deliberately explicit about every service involved. An updater that
// inferred the dependency set from the compose file would be reading application
// structure, and would break the next time that structure changed.
func BuildUpdatePlan(check *CheckResponse) *protocol.Plan {
	if check == nil {
		return nil
	}
	// Without a usable image reference the plan would be rejected outright. Omitting
	// it instead lets the updater synthesize its legacy single-service plan, which
	// keeps a deployment with an unusual image configuration working rather than
	// turning a previously degraded-but-functional case into a hard failure.
	if protocol.ImageRef(check.DockerImage, check.DockerTag) == "" {
		return nil
	}
	target := UpdaterTargetService()

	return &protocol.Plan{
		PlanVersion: protocol.MaxPlanVersion,
		Image:       check.DockerImage,
		Tag:         check.DockerTag,

		// The new build migrates its own deployment topology and environment before
		// anything is recreated. See internal/deploy/composeplan.
		Prepare: &protocol.PrepareStep{
			Command: []string{"clirelay-init-env"},
			Env: map[string]string{
				"CLIRELAY_TARGET_SERVICE": target,
			},
			// Older deployments have no init entrypoint in their image yet. Failing
			// the whole update over a migration they do not need would strand them.
			Optional: true,
		},

		Pull: []string{target},

		Stages: []protocol.Stage{
			{
				ID:          dependencyStageID,
				Services:    []string{envOrDefault("CLIRELAY_POSTGRES_SERVICE", defaultPostgresService), envOrDefault("CLIRELAY_REDIS_SERVICE", defaultRedisService)},
				WaitHealthy: true,
				// Skipped on deployments that predate the data stack rather than
				// failing them.
				Optional: true,
				Weight:   1,
			},
			{
				ID:       applicationStageID,
				Services: []string{target},
				// --no-deps keeps compose from restarting the database underneath a
				// running application when only the application image changed.
				NoDeps:         true,
				RemoveOrphans:  true,
				WaitHealthy:    true,
				TimeoutSeconds: applicationWaitTimeoutSeconds,
				Weight:         3,
			},
		},

		SelfRefreshService: envOrDefault("CLIRELAY_UPDATER_SERVICE", defaultUpdaterService),
	}
}

func envOrDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
