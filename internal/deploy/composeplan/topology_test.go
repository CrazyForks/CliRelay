package composeplan

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUpgradeComposeRuntimeStackPreservesListEnvironment(t *testing.T) {
	upgraded, _, err := UpgradeRuntimeStack(`
services:
  clirelay:
    image: ghcr.io/kittors/clirelay:dev
    environment:
      - AUTH_PATH=/root/.cli-proxy-api
      - LEGACY_FLAG=1
`, "/opt/clirelay", "clirelay")
	if err != nil {
		t.Fatalf("upgradeComposeRuntimeStack failed: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(upgraded), &doc); err != nil {
		t.Fatalf("parse upgraded compose: %v", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		t.Fatalf("services not found in upgraded compose:\n%s", upgraded)
	}
	clirelay, ok := stringMap(services["clirelay"])
	if !ok {
		t.Fatalf("clirelay service not found in upgraded compose:\n%s", upgraded)
	}
	env, ok := stringMap(clirelay["environment"])
	if !ok {
		t.Fatalf("clirelay environment is not a map:\n%s", upgraded)
	}
	for key, want := range map[string]any{
		"AUTH_PATH":   "/root/.cli-proxy-api",
		"LEGACY_FLAG": "1",
	} {
		if env[key] != want {
			t.Fatalf("environment[%s] = %v, want %v", key, env[key], want)
		}
	}
	for _, forbidden := range runtimeStackEnvKeys() {
		if _, ok := env[forbidden]; ok {
			t.Fatalf("environment still contains generated runtime key %s: %#v", forbidden, env)
		}
	}
	if got := clirelay["entrypoint"]; got == nil {
		t.Fatalf("clirelay service missing source-env entrypoint:\n%s", upgraded)
	}
	if !reflect.DeepEqual(clirelay["command"], []any{"./CLIProxyAPI"}) {
		t.Fatalf("clirelay command = %#v, want ./CLIProxyAPI\n%s", clirelay["command"], upgraded)
	}
	volumes, ok := clirelay["volumes"].([]any)
	if !ok || !containsAnyString(volumes, "${CLIRELAY_PROJECT_DIR:-/opt/clirelay}:/clirelay-deploy") {
		t.Fatalf("clirelay service missing /clirelay-deploy volume: %#v\n%s", clirelay["volumes"], upgraded)
	}
	for _, name := range []string{"clirelay-init", "postgres", "redis", "clirelay-updater"} {
		if _, ok := services[name]; !ok {
			t.Fatalf("upgraded compose missing service %s:\n%s", name, upgraded)
		}
	}
	if _, ok := services["clirelay-migrate"]; ok {
		t.Fatalf("upgraded compose still contains SQLite migration service:\n%s", upgraded)
	}
	for _, name := range []string{"clirelay-init", "clirelay-updater"} {
		service, ok := stringMap(services[name])
		if !ok {
			t.Fatalf("%s service not found:\n%s", name, upgraded)
		}
		healthcheck, ok := stringMap(service["healthcheck"])
		if !ok || healthcheck["disable"] != true {
			t.Fatalf("%s healthcheck = %#v, want disabled\n%s", name, service["healthcheck"], upgraded)
		}
	}
}

func TestImageFallbackUnwrapsCliProxyImageDefault(t *testing.T) {
	got := imageFallback("${CLI_PROXY_IMAGE:-ghcr.io/kittors/clirelay:latest}")
	if got != "ghcr.io/kittors/clirelay:latest" {
		t.Fatalf("imageFallback = %q, want literal image", got)
	}
}

func TestUpgradeComposeRuntimeStackKeepsGeneratedServicesOnTargetNetwork(t *testing.T) {
	upgraded, _, err := UpgradeRuntimeStack(`
services:
  cli-proxy-api:
    image: ghcr.io/kittors/clirelay:dev
    networks:
      - clirelay
networks:
  clirelay:
    name: clirelay
`, "/root/cliproxy", "cli-proxy-api")
	if err != nil {
		t.Fatalf("upgradeComposeRuntimeStack failed: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(upgraded), &doc); err != nil {
		t.Fatalf("parse upgraded compose: %v", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		t.Fatalf("services not found in upgraded compose:\n%s", upgraded)
	}
	target, ok := stringMap(services["cli-proxy-api"])
	if !ok {
		t.Fatalf("target service missing:\n%s", upgraded)
	}
	wantNetworks := target["networks"]
	for _, name := range []string{"postgres", "redis", "clirelay-updater"} {
		service, ok := stringMap(services[name])
		if !ok {
			t.Fatalf("service %s missing:\n%s", name, upgraded)
		}
		if !reflect.DeepEqual(service["networks"], wantNetworks) {
			t.Fatalf("%s networks = %#v, want %#v\n%s", name, service["networks"], wantNetworks, upgraded)
		}
	}
}

func TestEnsureRuntimeDataStackConfigUpgradesStackWithoutInitService(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	envPath := filepath.Join(dir, ".env")
	composeText := `services:
  clirelay:
    image: ghcr.io/kittors/clirelay:dev
    environment:
      CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN:?CLIRELAY_UPDATER_TOKEN is required for updater sidecar}
  postgres:
    image: postgres:15-alpine
  redis:
    image: redis:7-alpine
`
	if err := os.WriteFile(composePath, []byte(composeText), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	prepared := envPath
	err := Migrate(Options{ComposeFile: composePath, EnvFile: envPath, ProjectDir: filepath.Dir(composePath), Service: "clirelay"})
	if err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if prepared != envPath {
		t.Fatalf("prepared env path = %q, want %q", prepared, envPath)
	}
	upgradedData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	upgraded := string(upgradedData)
	for _, want := range []string{"clirelay-init:", "/clirelay-deploy/.env", "service_completed_successfully"} {
		if !strings.Contains(upgraded, want) {
			t.Fatalf("compose missing %q:\n%s", want, upgraded)
		}
	}
	if strings.Contains(upgraded, "clirelay-migrate:") {
		t.Fatalf("compose still contains SQLite migration service:\n%s", upgraded)
	}
	if strings.Contains(upgraded, "CLIRELAY_UPDATER_TOKEN is required") {
		t.Fatalf("compose still requires updater token:\n%s", upgraded)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	for _, want := range []string{"CLIRELAY_UPDATER_TOKEN=", "CLIRELAY_POSTGRES_DSN=postgres://", "CLIRELAY_REDIS_ENABLE=true"} {
		if !strings.Contains(string(envData), want) {
			t.Fatalf("env missing %q:\n%s", want, envData)
		}
	}
}

func TestEnsureRuntimeDataStackConfigRemovesLegacyMigrationService(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	envPath := filepath.Join(dir, ".env")
	composeText := `services:
  clirelay:
    image: ghcr.io/kittors/clirelay:dev
    depends_on:
      clirelay-migrate:
        condition: service_completed_successfully
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
  clirelay-init:
    image: ghcr.io/kittors/clirelay:dev
  clirelay-migrate:
    image: ghcr.io/kittors/clirelay:dev
    command: ["migrate-sqlite-to-postgres.sh"]
  postgres:
    image: postgres:15-alpine
  redis:
    image: redis:7-alpine
`
	if err := os.WriteFile(composePath, []byte(composeText), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}

	if err := Migrate(Options{ComposeFile: composePath, EnvFile: envPath, ProjectDir: filepath.Dir(composePath), Service: "clirelay"}); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	upgradedData, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	upgraded := string(upgradedData)
	if strings.Contains(upgraded, "clirelay-migrate") || strings.Contains(upgraded, "migrate-sqlite-to-postgres.sh") {
		t.Fatalf("compose still contains legacy SQLite migration wiring:\n%s", upgraded)
	}
}

func TestEnsureRuntimeEnvFileReplacesWorkspaceProjectDir(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(strings.Join([]string{
		"CLIRELAY_PROJECT_DIR=/workspace",
		"CLIRELAY_POSTGRES_DATA_PATH=/workspace/postgres-data",
		"CLIRELAY_REDIS_DATA_PATH=/workspace/redis-data",
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if err := ensureRuntimeEnvFile(envPath, "/root/cliproxy", "cli-proxy-api", "ghcr.io/kittors/clirelay:dev", "", false); err != nil {
		t.Fatalf("ensureRuntimeEnvFile failed: %v", err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"CLIRELAY_PROJECT_DIR=/root/cliproxy\n",
		"CLIRELAY_POSTGRES_DATA_PATH=/root/cliproxy/postgres-data\n",
		"CLIRELAY_REDIS_DATA_PATH=/root/cliproxy/redis-data\n",
		"AUTH_PATH=/CLIProxyAPI/auths\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("env missing %q:\n%s", want, content)
		}
	}
}

func TestEnsureRuntimeEnvFileAlignsStaleAuthPath(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("AUTH_PATH=/CLIProxyAPI/auths\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	// Preferred path comes from a concrete volume destination (legacy default).
	if err := ensureRuntimeEnvFile(envPath, "/root/cliproxy", "cli-proxy-api", "ghcr.io/kittors/clirelay:dev", "/root/.cli-proxy-api", true); err != nil {
		t.Fatalf("ensureRuntimeEnvFile failed: %v", err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(string(data), "AUTH_PATH=/root/.cli-proxy-api\n") {
		t.Fatalf("stale AUTH_PATH was not realigned:\n%s", data)
	}
}

func TestEnsureRuntimeEnvFileKeepsExistingAuthPathWhenNotForced(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("AUTH_PATH=/root/.cli-proxy-api\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	if err := ensureRuntimeEnvFile(envPath, "/root/cliproxy", "cli-proxy-api", "ghcr.io/kittors/clirelay:dev", "/CLIProxyAPI/auths", false); err != nil {
		t.Fatalf("ensureRuntimeEnvFile failed: %v", err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(string(data), "AUTH_PATH=/root/.cli-proxy-api\n") {
		t.Fatalf("existing AUTH_PATH should be preserved when not forced:\n%s", data)
	}
}

func TestUpgradeComposeRuntimeStackAlignsAuthPathWithLegacyVolume(t *testing.T) {
	upgraded, _, err := UpgradeRuntimeStack(`
services:
  cli-proxy-api:
    image: ghcr.io/kittors/clirelay:dev
    environment:
      AUTH_PATH: ${AUTH_PATH:-/CLIProxyAPI/auths}
    volumes:
      - ${CLI_PROXY_AUTH_PATH:-./auths}:/root/.cli-proxy-api
`, "/opt/clirelay", "cli-proxy-api")
	if err != nil {
		t.Fatalf("upgradeComposeRuntimeStack failed: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(upgraded), &doc); err != nil {
		t.Fatalf("parse upgraded compose: %v", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		t.Fatalf("services missing:\n%s", upgraded)
	}
	target, ok := stringMap(services["cli-proxy-api"])
	if !ok {
		t.Fatalf("target missing:\n%s", upgraded)
	}
	env, ok := stringMap(target["environment"])
	if !ok {
		t.Fatalf("environment missing:\n%s", upgraded)
	}
	if env["AUTH_PATH"] != "/root/.cli-proxy-api" {
		t.Fatalf("AUTH_PATH = %v, want /root/.cli-proxy-api\n%s", env["AUTH_PATH"], upgraded)
	}
}

func TestAlignAuthPathWithVolumesUsesInterpolationAwareSplit(t *testing.T) {
	// Volume dest is driven by AUTH_PATH itself; keep existing concrete AUTH_PATH.
	env, _ := alignAuthPathWithVolumes(map[string]any{
		"AUTH_PATH": "/root/.cli-proxy-api",
	}, []any{
		"${CLI_PROXY_AUTH_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/auths}:${AUTH_PATH:-/CLIProxyAPI/auths}",
	})
	if env["AUTH_PATH"] != "/root/.cli-proxy-api" {
		t.Fatalf("AUTH_PATH = %v, want preserved /root/.cli-proxy-api", env["AUTH_PATH"])
	}

	// Unset AUTH_PATH with AUTH_PATH-driven volume uses the volume default.
	env, _ = alignAuthPathWithVolumes(map[string]any{}, []any{
		"${CLI_PROXY_AUTH_PATH:-./auths}:${AUTH_PATH:-/root/.cli-proxy-api}",
	})
	if env["AUTH_PATH"] != "/root/.cli-proxy-api" {
		t.Fatalf("AUTH_PATH = %v, want default from volume dest", env["AUTH_PATH"])
	}

	// Concrete volume destination overrides a mismatched AUTH_PATH.
	env, _ = alignAuthPathWithVolumes(map[string]any{
		"AUTH_PATH": "/CLIProxyAPI/auths",
	}, []any{
		"./auths:/root/.cli-proxy-api",
	})
	if env["AUTH_PATH"] != "/root/.cli-proxy-api" {
		t.Fatalf("AUTH_PATH = %v, want concrete volume dest", env["AUTH_PATH"])
	}
}

func TestSplitComposeVolumePartsIgnoresColonsInInterpolation(t *testing.T) {
	parts := splitComposeVolumeParts("${CLI_PROXY_AUTH_PATH:-${PWD:-.}/auths}:${AUTH_PATH:-/CLIProxyAPI/auths}:ro")
	if len(parts) != 3 {
		t.Fatalf("parts = %#v, want 3 segments", parts)
	}
	if parts[0] != "${CLI_PROXY_AUTH_PATH:-${PWD:-.}/auths}" {
		t.Fatalf("source = %q", parts[0])
	}
	if parts[1] != "${AUTH_PATH:-/CLIProxyAPI/auths}" {
		t.Fatalf("dest = %q", parts[1])
	}
	if parts[2] != "ro" {
		t.Fatalf("mode = %q", parts[2])
	}
}

func containsAnyString(items []any, want string) bool {
	for _, item := range items {
		if stringValue(item) == want {
			return true
		}
	}
	return false
}
