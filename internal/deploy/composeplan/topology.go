// Package composeplan migrates a CliRelay deployment's docker-compose topology and
// environment file to what the current build needs.
//
// This logic used to live in the updater sidecar, which meant every change to the
// deployment's shape — a new service, a renamed container, another environment
// variable — required an updater release, and a stale updater would quietly write a
// topology the application could no longer run. Online update broke whenever the
// project was refactored.
//
// It lives here instead because this is the side that knows what the application
// needs. It runs from the newly pulled image as the clirelay-init container, before
// anything is recreated, so a build migrates its own deployment forward. The updater
// only knows that a preparation step exists, never what it does.
package composeplan

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultAppImage = "ghcr.io/kittors/clirelay:latest"

// DefaultAuthPath is the container-side auth directory current builds read.
const DefaultAuthPath = "/CLIProxyAPI/auths"

// Options configure a migration run.
type Options struct {
	ComposeFile string
	EnvFile     string
	ProjectDir  string
	Service     string

	// Logf receives human-readable progress. The init container's output is streamed
	// into the update console, so these lines are user-visible.
	Logf func(format string, args ...any)
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Migrate brings the deployment's compose file and env file up to date.
//
// It is idempotent: a deployment already on the current topology is left untouched,
// which matters because this runs on every update, not just the first.
func Migrate(options Options) error {
	composeFile := strings.TrimSpace(options.ComposeFile)
	if composeFile == "" {
		return nil
	}
	composeData, err := os.ReadFile(composeFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read docker compose file: %w", err)
	}

	envFile := strings.TrimSpace(options.EnvFile)
	if envFile == "" {
		envFile = filepath.Join(filepath.Dir(composeFile), ".env")
	}
	projectDir := strings.TrimSpace(options.ProjectDir)
	if projectDir == "" {
		projectDir = filepath.Dir(composeFile)
	}

	composeText := string(composeData)
	service := strings.TrimSpace(options.Service)

	// A deployment that already has the runtime stack and no leftover migration
	// service only needs its env file reconciled.
	hasRuntimeStack := HasComposeService(composeText, "postgres") &&
		HasComposeService(composeText, "redis") &&
		HasComposeService(composeText, "clirelay-init")
	if hasRuntimeStack && !HasComposeService(composeText, "clirelay-migrate") {
		preferredAuth, forceAuth := preferredAuthPathFromCompose(composeText, service)
		return ensureRuntimeEnvFile(envFile, projectDir, service, composeAppImage(composeText, service), preferredAuth, forceAuth)
	}

	options.logf("upgrading the docker compose runtime data stack")
	nextCompose, appImage, err := UpgradeRuntimeStack(composeText, projectDir, service)
	if err != nil {
		return err
	}
	preferredAuth, forceAuth := preferredAuthPathFromCompose(nextCompose, service)
	if err := ensureRuntimeEnvFile(envFile, projectDir, service, appImage, preferredAuth, forceAuth); err != nil {
		return err
	}
	if err := writeFile(composeFile, []byte(nextCompose), 0o644); err != nil {
		return fmt.Errorf("upgrade docker-compose.yml for the PostgreSQL/Redis runtime stack: %w", err)
	}
	options.logf("docker-compose.yml upgraded with the PostgreSQL/Redis runtime services")
	return nil
}

// UpgradeRuntimeStack rewrites a compose document so it carries the services the
// current build requires, returning the new document and the application image.
func UpgradeRuntimeStack(composeText string, projectDir string, service string) (string, string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(composeText), &doc); err != nil {
		return "", "", fmt.Errorf("parse docker compose file: %w", err)
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		return "", "", fmt.Errorf("docker compose file has no services section")
	}

	targetName := strings.TrimSpace(service)
	if _, ok := services[targetName]; !ok {
		targetName = firstApplicationService(services)
	}
	if targetName == "" {
		return "", "", fmt.Errorf("docker compose file has no CliRelay service to upgrade")
	}
	target, ok := stringMap(services[targetName])
	if !ok {
		target = map[string]any{}
		services[targetName] = target
	}

	appImage := imageFallback(stringValue(target["image"]))
	if appImage == "" {
		appImage = defaultAppImage
	}
	target["image"] = "${CLI_PROXY_IMAGE:-" + appImage + "}"
	target["entrypoint"] = sourceEnvEntrypoint()
	if !hasComposeCommand(target["command"]) {
		target["command"] = []any{"./CLIProxyAPI"}
	}

	targetEnv := withoutEnvKeys(target["environment"], runtimeStackEnvKeys()...)
	// Keep AUTH_PATH aligned with the existing auth volume destination so an upgrade
	// does not leave host auth files mounted at a path the process no longer reads.
	targetEnv, target["volumes"] = alignAuthPathWithVolumes(targetEnv, target["volumes"])
	target["environment"] = targetEnv
	target["volumes"] = appendVolume(target["volumes"], "${CLIRELAY_PROJECT_DIR:-"+projectDir+"}:/clirelay-deploy")

	targetNetworks := target["networks"]
	target["depends_on"] = map[string]any{
		"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
		"postgres":      map[string]any{"condition": "service_healthy"},
		"redis":         map[string]any{"condition": "service_healthy"},
	}
	services["clirelay-init"] = initComposeService(projectDir, targetName, appImage)
	services["postgres"] = postgresComposeService()
	services["redis"] = redisComposeService()
	services["clirelay-updater"] = updaterComposeService(projectDir, targetName, appImage)
	delete(services, "clirelay-migrate")

	if targetNetworks != nil {
		for _, name := range []string{"clirelay-init", "postgres", "redis", "clirelay-updater"} {
			if generated, ok := stringMap(services[name]); ok {
				generated["networks"] = targetNetworks
			}
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", "", fmt.Errorf("render the upgraded docker compose file: %w", err)
	}
	return string(out), appImage, nil
}

func composeAppImage(composeText string, service string) string {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(composeText), &doc); err != nil {
		return defaultAppImage
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		return defaultAppImage
	}
	targetName := strings.TrimSpace(service)
	if _, ok := services[targetName]; !ok {
		targetName = firstApplicationService(services)
	}
	if target, ok := stringMap(services[targetName]); ok {
		if image := stringValue(target["image"]); image != "" {
			return imageFallback(image)
		}
	}
	return defaultAppImage
}

func firstApplicationService(services map[string]any) string {
	for name := range services {
		if name != "postgres" && name != "redis" && name != "clirelay-init" &&
			name != "clirelay-migrate" && !strings.Contains(name, "updater") {
			return name
		}
	}
	return ""
}

func initComposeService(projectDir string, targetService string, image string) map[string]any {
	return map[string]any{
		"image":   "${CLI_PROXY_IMAGE:-" + image + "}",
		"command": []any{"clirelay-init-env"},
		"environment": map[string]any{
			"CLI_PROXY_IMAGE":               "${CLI_PROXY_IMAGE:-" + image + "}",
			"CLIRELAY_PROJECT_DIR":          "${CLIRELAY_PROJECT_DIR:-" + projectDir + "}",
			"CLIRELAY_ENV_FILE":             "/clirelay-deploy/.env",
			"CLIRELAY_COMPOSE_FILE":         "/clirelay-deploy/docker-compose.yml",
			"CLIRELAY_COMPOSE_PROJECT_NAME": "${CLIRELAY_COMPOSE_PROJECT_NAME:-" + filepath.Base(projectDir) + "}",
			"CLIRELAY_TARGET_SERVICE":       "${CLIRELAY_TARGET_SERVICE:-" + targetService + "}",
		},
		"volumes":     []any{"${CLIRELAY_PROJECT_DIR:-" + projectDir + "}:/clirelay-deploy"},
		"healthcheck": map[string]any{"disable": true},
		"restart":     "no",
	}
}

func postgresComposeService() map[string]any {
	return map[string]any{
		"image":      "postgres:15-alpine",
		"entrypoint": []any{"sh", "-c", "set -a; . /clirelay-deploy/.env; set +a; export POSTGRES_DB=\"$$CLIRELAY_POSTGRES_DB\" POSTGRES_USER=\"$$CLIRELAY_POSTGRES_USER\" POSTGRES_PASSWORD=\"$$CLIRELAY_POSTGRES_PASSWORD\"; exec docker-entrypoint.sh postgres"},
		"volumes": []any{
			"${CLIRELAY_POSTGRES_DATA_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/postgres-data}:/var/lib/postgresql/data",
			"${CLIRELAY_PROJECT_DIR:-${PWD:-.}}:/clirelay-deploy",
		},
		"healthcheck": map[string]any{
			"test":     []any{"CMD-SHELL", ". /clirelay-deploy/.env; pg_isready -U \"$$CLIRELAY_POSTGRES_USER\" -d \"$$CLIRELAY_POSTGRES_DB\""},
			"interval": "5s",
			"timeout":  "5s",
			"retries":  20,
		},
		"depends_on": map[string]any{
			"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
		},
		"restart": "unless-stopped",
	}
}

func redisComposeService() map[string]any {
	return map[string]any{
		"image":   "redis:7-alpine",
		"command": []any{"redis-server", "--appendonly", "yes"},
		"volumes": []any{"${CLIRELAY_REDIS_DATA_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/redis-data}:/data"},
		"healthcheck": map[string]any{
			"test":     []any{"CMD", "redis-cli", "ping"},
			"interval": "5s",
			"timeout":  "5s",
			"retries":  20,
		},
		"depends_on": map[string]any{
			"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
		},
		"restart": "unless-stopped",
	}
}

func updaterComposeService(projectDir string, targetService string, image string) map[string]any {
	return map[string]any{
		"image":      "${CLI_PROXY_IMAGE:-" + image + "}",
		"command":    []any{"./clirelay-updater"},
		"entrypoint": []any{"sh", "-c", "set -a; . /clirelay-deploy/.env; set +a; exec docker-entrypoint.sh ./clirelay-updater"},
		"user":       "0:0",
		"environment": map[string]any{
			"CLIRELAY_PROJECT_DIR":          "${CLIRELAY_PROJECT_DIR:-" + projectDir + "}",
			"CLIRELAY_COMPOSE_FILE":         "${CLIRELAY_PROJECT_DIR:-" + projectDir + "}/docker-compose.yml",
			"CLIRELAY_ENV_FILE":             "${CLIRELAY_ENV_FILE:-${CLIRELAY_PROJECT_DIR:-" + projectDir + "}/.env}",
			"CLIRELAY_COMPOSE_PROJECT_NAME": "${CLIRELAY_COMPOSE_PROJECT_NAME:-}",
			"CLIRELAY_TARGET_SERVICE":       "${CLIRELAY_TARGET_SERVICE:-" + targetService + "}",
			"CLIRELAY_UPDATER_STATE_FILE":   "${CLIRELAY_UPDATER_STATE_FILE:-${CLIRELAY_PROJECT_DIR:-" + projectDir + "}/.clirelay-updater-status.json}",
		},
		"volumes": []any{
			"/var/run/docker.sock:/var/run/docker.sock",
			"${CLIRELAY_PROJECT_DIR:-" + projectDir + "}:${CLIRELAY_PROJECT_DIR:-" + projectDir + "}",
			"${CLIRELAY_PROJECT_DIR:-" + projectDir + "}:/clirelay-deploy",
		},
		"depends_on": map[string]any{
			"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
		},
		"healthcheck": map[string]any{"disable": true},
		"restart":     "unless-stopped",
	}
}

func sourceEnvEntrypoint() []any {
	return []any{"sh", "-c", "set -a; . /clirelay-deploy/.env; set +a; exec docker-entrypoint.sh \"$@\"", "--"}
}

func hasComposeCommand(value any) bool {
	if strings.TrimSpace(stringValue(value)) != "" {
		return true
	}
	if items, ok := value.([]any); ok {
		return len(items) > 0
	}
	if items, ok := value.([]string); ok {
		return len(items) > 0
	}
	return false
}

// runtimeStackEnvKeys are the keys the env file owns. They are stripped from the
// service's inline environment so the two cannot drift apart and disagree.
func runtimeStackEnvKeys() []string {
	return []string{
		"CLIRELAY_POSTGRES_DSN",
		"CLIRELAY_REDIS_ENABLE",
		"CLIRELAY_REDIS_ADDR",
		"CLIRELAY_REDIS_PASSWORD",
		"CLIRELAY_REDIS_DB",
		"CLIRELAY_TARGET_SERVICE",
		"CLIRELAY_UPDATER_URL",
		"CLIRELAY_UPDATER_TOKEN",
		"CLIRELAY_UPDATER_STATE_FILE",
		"CLIRELAY_SQLITE_AUTO_MIGRATE",
		"CLIRELAY_SQLITE_AUTO_IMPORT",
		"CLIRELAY_SQLITE_PATH",
	}
}

func withoutEnvKeys(existing any, keys ...string) map[string]any {
	env := mergeEnv(existing, nil)
	for _, key := range keys {
		delete(env, key)
	}
	return env
}

func mergeEnv(existing any, values map[string]any) map[string]any {
	merged := map[string]any{}
	if current, ok := stringMap(existing); ok {
		for key, value := range current {
			merged[key] = value
		}
	} else if current, ok := existing.([]any); ok {
		for _, item := range current {
			key, value, ok := strings.Cut(stringValue(item), "=")
			if ok && strings.TrimSpace(key) != "" {
				merged[strings.TrimSpace(key)] = value
			}
		}
	}
	for key, value := range values {
		merged[key] = value
	}
	return merged
}

func appendVolume(existing any, volume string) []any {
	var volumes []any
	if current, ok := existing.([]any); ok {
		volumes = append(volumes, current...)
	} else if current, ok := existing.([]string); ok {
		for _, item := range current {
			volumes = append(volumes, item)
		}
	}
	for _, item := range volumes {
		if stringValue(item) == volume {
			return volumes
		}
	}
	return append(volumes, volume)
}

func stringMap(value any) (map[string]any, bool) {
	out, ok := value.(map[string]any)
	return out, ok
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func imageFallback(image string) string {
	text := strings.TrimSpace(image)
	const prefix = "${CLI_PROXY_IMAGE:-"
	if strings.HasPrefix(text, prefix) && strings.HasSuffix(text, "}") {
		if fallback := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, prefix), "}")); fallback != "" {
			return fallback
		}
	}
	return text
}

// HasComposeService reports whether a compose document declares a service. It scans
// text rather than parsing so a document with unusual anchors still answers.
func HasComposeService(content string, service string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == service+":" && len(line) > len(strings.TrimLeft(line, " \t")) {
			return true
		}
	}
	return false
}

func ensureRuntimeEnvFile(envFile string, projectDir string, service string, image string, preferredAuthPath string, forceAuthPath bool) error {
	data, err := os.ReadFile(envFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read docker env file: %w", err)
	}
	lines := splitEnvLines(string(data))
	values := envValues(lines)

	setEnvDefault(&lines, values, "CLI_PROXY_IMAGE", image)
	setEnvDefaultOrReplaceWorkspace(&lines, values, "CLIRELAY_PROJECT_DIR", projectDir)
	setEnvDefault(&lines, values, "CLIRELAY_TARGET_SERVICE", service)
	setEnvDefault(&lines, values, "CLIRELAY_COMPOSE_PROJECT_NAME", filepath.Base(projectDir))
	setEnvDefault(&lines, values, "CLIRELAY_UPDATER_TOKEN", envOrDefault("CLIRELAY_UPDATER_TOKEN", randomHex(16)))
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_DB", "cliproxy")
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_USER", "cliproxy")
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_PASSWORD", randomHex(16))

	db := valueOrDefault(values["CLIRELAY_POSTGRES_DB"], "cliproxy")
	user := valueOrDefault(values["CLIRELAY_POSTGRES_USER"], "cliproxy")
	pass := valueOrDefault(values["CLIRELAY_POSTGRES_PASSWORD"], "cliproxy")
	setEnvDefault(&lines, values, "CLIRELAY_POSTGRES_DSN", "postgres://"+user+":"+pass+"@postgres:5432/"+db+"?sslmode=disable")
	setEnvDefaultOrReplaceWorkspace(&lines, values, "CLIRELAY_POSTGRES_DATA_PATH", filepath.Join(projectDir, "postgres-data"))
	setEnvDefault(&lines, values, "CLIRELAY_REDIS_ENABLE", "true")
	setEnvDefault(&lines, values, "CLIRELAY_REDIS_ADDR", "redis:6379")
	setEnvDefault(&lines, values, "CLIRELAY_REDIS_DB", "0")
	setEnvDefaultOrReplaceWorkspace(&lines, values, "CLIRELAY_REDIS_DATA_PATH", filepath.Join(projectDir, "redis-data"))

	// Align AUTH_PATH with a concrete compose auth volume destination when forced;
	// otherwise only fill a missing AUTH_PATH so existing .env values stay valid.
	ensureRuntimeEnvAuthPath(&lines, values, preferredAuthPath, forceAuthPath)

	if err := writeFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write docker env file: %w", err)
	}
	return nil
}

func envValues(lines []string) map[string]string {
	values := map[string]string{}
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return values
}

func setEnvDefault(lines *[]string, values map[string]string, key string, value string) {
	if strings.TrimSpace(values[key]) != "" {
		return
	}
	*lines = append(*lines, key+"="+value)
	values[key] = value
}

// setEnvDefaultOrReplaceWorkspace also replaces a value pointing inside /workspace.
// Those come from a build-time path that does not exist on the deployment host, so
// leaving one in place produces a container that mounts an empty directory.
func setEnvDefaultOrReplaceWorkspace(lines *[]string, values map[string]string, key string, value string) {
	if existing := strings.TrimSpace(values[key]); existing != "" && !isWorkspacePath(existing) {
		return
	}
	for index, line := range *lines {
		currentKey, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(currentKey) == key {
			(*lines)[index] = key + "=" + value
			values[key] = value
			return
		}
	}
	*lines = append(*lines, key+"="+value)
	values[key] = value
}

func isWorkspacePath(value string) bool {
	clean := filepath.Clean(value)
	return clean == "/workspace" || strings.HasPrefix(clean, "/workspace"+string(os.PathSeparator))
}

func splitEnvLines(content string) []string {
	trimmed := strings.TrimRight(content, "\r\n")
	if trimmed == "" {
		return nil
	}
	raw := strings.Split(trimmed, "\n")
	lines := raw[:0]
	for _, line := range raw {
		lines = append(lines, strings.TrimRight(line, "\r"))
	}
	return lines
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func envOrDefault(key string, fallback string) string {
	return valueOrDefault(os.Getenv(key), fallback)
}

func valueOrDefault(value string, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}
