package updateflow

import (
	"testing"

	"clirelay.local/updater/protocol"
)

func validCheck() *CheckResponse {
	return &CheckResponse{
		DockerImage: "ghcr.io/kittors/clirelay",
		DockerTag:   "dev",
	}
}

func TestBuildUpdatePlanIsValidAndTargetsTheConfiguredService(t *testing.T) {
	t.Setenv("CLIRELAY_TARGET_SERVICE", "cli-proxy-api")

	plan := BuildUpdatePlan(validCheck())
	if plan == nil {
		t.Fatal("no plan was built")
	}
	normalized, err := plan.Validate()
	if err != nil {
		t.Fatalf("the plan the application emits must pass the updater's validation: %v", err)
	}

	if len(normalized.Pull) != 1 || normalized.Pull[0] != "cli-proxy-api" {
		t.Errorf("pull list = %v, want the configured service", normalized.Pull)
	}
	if len(normalized.Stages) != 2 {
		t.Fatalf("stage count = %d, want dependencies then application", len(normalized.Stages))
	}
	if normalized.Stages[0].ID != dependencyStageID || !normalized.Stages[0].Optional {
		t.Error("the dependency stage must come first and be optional for older deployments")
	}

	application := normalized.Stages[1]
	if application.ID != applicationStageID || application.Services[0] != "cli-proxy-api" {
		t.Errorf("application stage = %+v", application)
	}
	// --no-deps keeps compose from restarting the database underneath the app.
	if !application.NoDeps {
		t.Error("the application stage must not drag its dependencies into recreation")
	}
	if !application.WaitHealthy {
		t.Error("the application stage must wait for health before the update is called done")
	}
}

// TestBuildUpdatePlanOmitsItselfWithoutAUsableImage covers the degraded case: a plan
// that cannot name a valid image would be rejected by the updater, so none is sent
// and the updater falls back to its legacy single-service behaviour.
func TestBuildUpdatePlanOmitsItselfWithoutAUsableImage(t *testing.T) {
	for _, check := range []*CheckResponse{
		nil,
		{},
		{DockerImage: "ghcr.io/kittors/clirelay"},
		{DockerTag: "dev"},
		{DockerImage: "ghcr.io/kittors/clirelay", DockerTag: "dev`whoami`"},
	} {
		if plan := BuildUpdatePlan(check); plan != nil {
			t.Errorf("BuildUpdatePlan(%+v) = %+v, want nil so the updater falls back", check, plan)
		}
	}
}

func TestBuildUpdatePlanRefreshesTheUpdaterSidecar(t *testing.T) {
	plan := BuildUpdatePlan(validCheck())
	if plan.SelfRefreshService != defaultUpdaterService {
		t.Errorf("SelfRefreshService = %q, want %q", plan.SelfRefreshService, defaultUpdaterService)
	}
}

// TestBuildUpdatePlanPreparesViaAnAllowlistedCommand pins that the application only
// ever asks for a preparation command the updater is willing to run. If this drifts,
// online update fails closed at the updater rather than silently skipping migration.
func TestBuildUpdatePlanPreparesViaAnAllowlistedCommand(t *testing.T) {
	plan := BuildUpdatePlan(validCheck())
	if plan.Prepare == nil {
		t.Fatal("no preparation step was declared")
	}
	if _, ok := protocol.PrepareCommandAllowlist[plan.Prepare.Command[0]]; !ok {
		t.Errorf("prepare command %q is not in the updater's allowlist", plan.Prepare.Command[0])
	}
	// Older images have no init entrypoint; failing their update over a migration
	// they do not need would strand them on the old version.
	if !plan.Prepare.Optional {
		t.Error("preparation must be optional so deployments predating it can still update")
	}
}

func TestBuildUpdatePlanHonoursServiceNameOverrides(t *testing.T) {
	t.Setenv("CLIRELAY_POSTGRES_SERVICE", "db")
	t.Setenv("CLIRELAY_REDIS_SERVICE", "cache")
	t.Setenv("CLIRELAY_UPDATER_SERVICE", "my-updater")

	plan := BuildUpdatePlan(validCheck())
	if got := plan.Stages[0].Services; got[0] != "db" || got[1] != "cache" {
		t.Errorf("dependency services = %v, want the overrides", got)
	}
	if plan.SelfRefreshService != "my-updater" {
		t.Errorf("SelfRefreshService = %q, want my-updater", plan.SelfRefreshService)
	}
}
