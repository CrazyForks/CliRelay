package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"clirelay.local/updater/protocol"
)

// ImageEnvKey is the environment variable the deployment interpolates into every
// service's image reference. Pointing it at a new tag is the whole mechanism by
// which an update takes effect, which is why it is the one deployment value the
// updater still writes.
const ImageEnvKey = "CLI_PROXY_IMAGE"

// ErrImageNotAllowed is returned when a plan asks for an image outside the
// repository the deployment is already configured with.
var ErrImageNotAllowed = errors.New("requested image is not allowed")

// ReadConfiguredImage returns the image reference currently pinned in the env file.
func ReadConfiguredImage(envFile string) string {
	if strings.TrimSpace(envFile) == "" {
		return ""
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		return ""
	}
	return configuredImageRef(SplitEnvLines(string(data)))
}

// PersistImage pins the target image in the env file.
//
// The repository is compared against whatever is already configured and a mismatch
// is refused. Without that check, anyone able to reach the management API could
// point the deployment at an arbitrary registry and have the updater pull and run
// it with the Docker socket mounted. Moving between tags of the configured
// repository is the only transition allowed.
func PersistImage(ctx context.Context, envFile string, image string, tag string, reporter Reporter) error {
	imageRef := protocol.ImageRef(image, tag)
	if imageRef == "" {
		if strings.TrimSpace(image) == "" && strings.TrimSpace(tag) == "" {
			return nil
		}
		return fmt.Errorf("%w: invalid image or tag", ErrImageNotAllowed)
	}
	if strings.TrimSpace(envFile) == "" {
		return nil
	}

	data, err := os.ReadFile(envFile)
	if os.IsNotExist(err) {
		return WriteHostFile(ctx, envFile, []byte(ImageEnvKey+"="+imageRef+"\n"), 0o600, reporter)
	}
	if err != nil {
		return err
	}

	lines := SplitEnvLines(string(data))
	configuredRepo := protocol.ImageRepository(configuredImageRef(lines))
	requestedRepo := protocol.ImageRepository(imageRef)
	if configuredRepo == "" {
		lines = append(lines, ImageEnvKey+"="+imageRef)
		return WriteHostFile(ctx, envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600, reporter)
	}
	if requestedRepo != configuredRepo {
		return fmt.Errorf("%w: %s does not match %s", ErrImageNotAllowed, requestedRepo, configuredRepo)
	}
	return WriteEnvKey(ctx, envFile, ImageEnvKey, imageRef, reporter)
}

// RestoreImage puts a previous image reference back after a failed update and folds
// any restore error into the original one, so the caller still reports the real
// cause rather than the cleanup failure.
func RestoreImage(ctx context.Context, envFile string, imageRef string, reporter Reporter, updateErr error) error {
	if strings.TrimSpace(envFile) == "" || strings.TrimSpace(imageRef) == "" {
		return updateErr
	}
	if err := WriteEnvKey(ctx, envFile, ImageEnvKey, imageRef, reporter); err != nil {
		return fmt.Errorf("%w; restoring the previous %s failed: %v", updateErr, ImageEnvKey, err)
	}
	reporter.Log("stdout", "restored the previous "+ImageEnvKey+" after a failed update")
	return updateErr
}

// WriteEnvKey sets a single key in the env file, preserving every other line.
func WriteEnvKey(ctx context.Context, envFile string, key string, value string, reporter Reporter) error {
	data, err := os.ReadFile(envFile)
	if err != nil {
		return err
	}
	lines := SplitEnvLines(string(data))
	line := key + "=" + value
	for index, existing := range lines {
		currentKey, _, ok := strings.Cut(existing, "=")
		if ok && strings.TrimSpace(currentKey) == key {
			lines[index] = line
			return WriteHostFile(ctx, envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600, reporter)
		}
	}
	lines = append(lines, line)
	return WriteHostFile(ctx, envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600, reporter)
}

// SplitEnvLines splits env file content into lines with trailing newlines removed.
func SplitEnvLines(content string) []string {
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

func configuredImageRef(lines []string) string {
	for _, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != ImageEnvKey {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}
