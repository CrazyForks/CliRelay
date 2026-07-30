package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Reporter receives progress and log output from a running update.
type Reporter interface {
	Stage(stage string, message string)
	Progress(update ProgressUpdate)
	Log(stream string, message string)
}

// ProgressUpdate is one progress observation. Percent is authoritative when
// non-negative; otherwise the receiver derives one from the stage weights.
type ProgressUpdate struct {
	Stage       string
	MessageCode string
	Message     string
	Percent     float64
	Bytes       int64
	TotalBytes  int64
	Stages      []StageState
}

// StageState mirrors one entry of the run's stage timeline.
type StageState struct {
	ID    string
	State string
}

// Docker wraps the docker CLI. Everything the updater does to a deployment goes
// through compose commands built from explicit service names — the compose file is
// never parsed here. That is what keeps the updater independent of how the
// application's deployment is structured.
type Docker struct {
	ComposeFile string
	EnvFile     string
	ProjectName string
}

// ComposeArgs builds a `docker compose ...` argument list.
func (d Docker) ComposeArgs(args ...string) []string {
	base := []string{"compose"}
	if strings.TrimSpace(d.ProjectName) != "" {
		base = append(base, "--project-name", d.ProjectName)
	}
	if strings.TrimSpace(d.EnvFile) != "" {
		base = append(base, "--env-file", d.EnvFile)
	}
	if strings.TrimSpace(d.ComposeFile) != "" {
		base = append(base, "-f", d.ComposeFile)
	}
	return append(base, args...)
}

// RunCompose executes a compose command, streaming both output streams to the
// reporter and to onLine as they arrive.
func (d Docker) RunCompose(ctx context.Context, reporter Reporter, onLine func(string), args ...string) error {
	full := d.ComposeArgs(args...)
	reporter.Log("stdout", "$ docker "+strings.Join(full, " "))

	cmd := exec.CommandContext(ctx, "docker", full...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamLines(stdout, "stdout", reporter, onLine, &wg)
	go streamLines(stderr, "stderr", reporter, onLine, &wg)

	waitErr := cmd.Wait()
	wg.Wait()
	if waitErr != nil {
		return fmt.Errorf("docker compose %s failed: %w", strings.Join(args, " "), waitErr)
	}
	return nil
}

func streamLines(reader io.Reader, stream string, reporter Reporter, onLine func(string), wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if onLine != nil {
			onLine(line)
		}
		reporter.Log(stream, line)
	}
	if err := scanner.Err(); err != nil {
		reporter.Log("stderr", "log stream error: "+err.Error())
	}
}

// ServiceNames asks compose which services the deployment defines.
//
// Asking compose rather than parsing the YAML matters: compose resolves overrides,
// profiles, extends and interpolation, and it stays correct when the application
// reorganizes its deployment. A parse here would be exactly the coupling this
// module exists to avoid.
func (d Docker) ServiceNames(ctx context.Context) (map[string]struct{}, error) {
	if strings.TrimSpace(d.ComposeFile) == "" {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "docker", d.ComposeArgs("config", "--services")...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list compose services: %w", err)
	}
	names := map[string]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names[name] = struct{}{}
		}
	}
	return names, nil
}

// RunDetached starts a container that outlives this process and returns its id.
func RunDetached(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RunOneShot runs a container to completion, streaming its output to the reporter.
func RunOneShot(ctx context.Context, reporter Reporter, args ...string) error {
	reporter.Log("stdout", "$ docker "+strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			reporter.Log("stdout", line)
		}
	}
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}

type dockerMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// Inspect returns a formatted field from `docker inspect` for a container.
func Inspect(ctx context.Context, containerID string, format string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", format, containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s failed: %w: %s", containerID, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteHostFile writes a deployment file, falling back to a helper container when
// the updater's own filesystem view is read-only or the path is not writable.
//
// The fallback exists because the sidecar frequently sees the project directory
// through a mount whose ownership does not permit a direct write, while the host
// path behind that mount does. Losing this fallback silently breaks updates on
// deployments that worked before.
func WriteHostFile(ctx context.Context, path string, data []byte, mode os.FileMode, reporter Reporter) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, mode); err == nil {
		return nil
	} else if fallbackErr := writeHostFileViaDocker(ctx, path, data, mode, reporter); fallbackErr != nil {
		return fmt.Errorf("%v; docker fallback failed: %w", err, fallbackErr)
	}
	return nil
}

func writeHostFileViaDocker(ctx context.Context, path string, data []byte, mode os.FileMode, reporter Reporter) error {
	containerID, err := os.Hostname()
	if err != nil || strings.TrimSpace(containerID) == "" {
		return fmt.Errorf("detect updater container id: %w", err)
	}
	image, err := Inspect(ctx, containerID, "{{.Config.Image}}")
	if err != nil {
		return err
	}
	mounts, err := containerMounts(ctx, containerID)
	if err != nil {
		return err
	}
	source, rel, dirMount, ok := HostPathForMountedPath(path, mounts)
	if !ok {
		return fmt.Errorf("no writable host mount found for %s", path)
	}
	reporter.Log("stdout", "direct write failed; updating deployment file through docker mount fallback")

	modeText := fmt.Sprintf("%#o", mode.Perm())
	var cmd *exec.Cmd
	if dirMount {
		script := `set -eu; target="/host/$TARGET_REL"; mkdir -p "$(dirname "$target")"; cat > "$target"; chmod "$TARGET_MODE" "$target"`
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
			"-e", "TARGET_REL="+rel, "-e", "TARGET_MODE="+modeText,
			"-v", source+":/host", strings.TrimSpace(image), "sh", "-c", script)
	} else {
		script := `set -eu; cat > /target; chmod "$TARGET_MODE" /target`
		cmd = exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
			"-e", "TARGET_MODE="+modeText,
			"-v", source+":/target", strings.TrimSpace(image), "sh", "-c", script)
	}
	cmd.Stdin = strings.NewReader(string(data))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker helper write failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func containerMounts(ctx context.Context, containerID string) ([]dockerMount, error) {
	mountsJSON, err := Inspect(ctx, containerID, "{{json .Mounts}}")
	if err != nil {
		return nil, err
	}
	var mounts []dockerMount
	if err := json.Unmarshal([]byte(mountsJSON), &mounts); err != nil {
		return nil, fmt.Errorf("parse updater container mounts: %w", err)
	}
	return mounts, nil
}

// HostPathForMountedPath maps a path inside this container back to the host path
// behind the mount that contains it. It returns the mount source, the path relative
// to the mount destination, whether the mount is a directory, and whether a mount
// was found at all.
func HostPathForMountedPath(path string, mounts []dockerMount) (string, string, bool, bool) {
	cleanPath := filepath.Clean(path)
	var best dockerMount
	bestLen := -1
	for _, mount := range mounts {
		if strings.TrimSpace(mount.Source) == "" || strings.TrimSpace(mount.Destination) == "" {
			continue
		}
		dest := filepath.Clean(mount.Destination)
		if cleanPath == dest {
			return mount.Source, "", false, true
		}
		if strings.HasPrefix(cleanPath, dest+string(os.PathSeparator)) && len(dest) > bestLen {
			best = mount
			bestLen = len(dest)
		}
	}
	if bestLen < 0 {
		return "", "", false, false
	}
	rel, err := filepath.Rel(filepath.Clean(best.Destination), cleanPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", false, false
	}
	return best.Source, rel, true, true
}

// ProjectDir resolves the host-side project directory for a compose file. Inside a
// container the compose file often lives under a mount, and docker needs host paths
// for any bind mount it is asked to create.
func ProjectDir(ctx context.Context, composeFile string) string {
	projectDir := filepath.Dir(composeFile)
	if !strings.HasPrefix(filepath.Clean(composeFile), "/workspace"+string(os.PathSeparator)) {
		return projectDir
	}
	containerID, err := os.Hostname()
	if err != nil || strings.TrimSpace(containerID) == "" {
		return projectDir
	}
	mounts, err := containerMounts(ctx, containerID)
	if err != nil {
		return projectDir
	}
	source, rel, dirMount, ok := HostPathForMountedPath(composeFile, mounts)
	if !ok {
		return projectDir
	}
	if !dirMount {
		return filepath.Dir(source)
	}
	return filepath.Dir(filepath.Join(source, rel))
}
