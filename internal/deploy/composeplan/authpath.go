package composeplan

import (
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// alignAuthPathWithVolumes ensures AUTH_PATH matches the container-side destination
// of the auth bind mount. Older compose files used /root/.cli-proxy-api; newer
// templates default to /CLIProxyAPI/auths. When both disagree after an upgrade,
// the host auths directory can still have files while the process reads an empty dir.
func alignAuthPathWithVolumes(env map[string]any, volumes any) (map[string]any, any) {
	if env == nil {
		env = map[string]any{}
	}
	rawAuth := strings.TrimSpace(stringValue(env["AUTH_PATH"]))
	authPath := normalizeAuthPathValue(rawAuth)
	dest, destIsAuthPathDriven, ok := findAuthVolumeDestination(volumes)
	if ok && !destIsAuthPathDriven {
		// Concrete mount target (e.g. /root/.cli-proxy-api) is authoritative.
		env["AUTH_PATH"] = dest
		return env, volumes
	}
	if ok && destIsAuthPathDriven {
		// Volume dest is ${AUTH_PATH:-...}; after compose interpolation the mount
		// target equals AUTH_PATH. Keep a concrete AUTH_PATH, prefer existing value.
		if authPath != "" && !strings.HasPrefix(authPath, "${") {
			env["AUTH_PATH"] = authPath
			return env, volumes
		}
		if dest != "" {
			env["AUTH_PATH"] = dest
			return env, volumes
		}
	}
	if authPath == "" || strings.HasPrefix(authPath, "${") {
		env["AUTH_PATH"] = "/CLIProxyAPI/auths"
		return env, volumes
	}
	env["AUTH_PATH"] = authPath
	return env, volumes
}

// findAuthVolumeDestination returns the container-side auth mount destination.
// destIsAuthPathDriven is true when dest is ${AUTH_PATH...} so the real path
// is whatever AUTH_PATH resolves to at compose time.
func findAuthVolumeDestination(volumes any) (dest string, destIsAuthPathDriven bool, ok bool) {
	for _, item := range volumeItems(volumes) {
		source, volumeDest, splitOK := splitBindVolume(stringValue(item))
		if !splitOK {
			continue
		}
		if !isAuthVolumeSource(source) && !isAuthVolumeDestination(volumeDest) {
			continue
		}
		if isAuthPathInterpolation(volumeDest) {
			// Prefer the default inside ${AUTH_PATH:-/path} when AUTH_PATH is unset.
			if literal, litOK := composeDefaultLiteral(volumeDest); litOK && literal != "" {
				return literal, true, true
			}
			return "", true, true
		}
		normalized := normalizeAuthPathValue(volumeDest)
		if normalized == "" || strings.HasPrefix(normalized, "${") {
			continue
		}
		return normalized, false, true
	}
	return "", false, false
}

func isAuthPathInterpolation(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "${AUTH_PATH") || strings.HasPrefix(trimmed, "${auth_path")
}

func volumeItems(volumes any) []any {
	if current, ok := volumes.([]any); ok {
		return current
	}
	if current, ok := volumes.([]string); ok {
		out := make([]any, 0, len(current))
		for _, item := range current {
			out = append(out, item)
		}
		return out
	}
	return nil
}

func splitBindVolume(spec string) (source string, dest string, ok bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "", false
	}
	// Short syntax "src:dst[:mode]" must not split on colons inside ${VAR:-default}.
	parts := splitComposeVolumeParts(spec)
	if len(parts) < 2 {
		return "", "", false
	}
	source = strings.TrimSpace(parts[0])
	dest = strings.TrimSpace(parts[1])
	if source == "" || dest == "" {
		return "", "", false
	}
	if dest == "ro" || dest == "rw" || dest == "z" || dest == "Z" {
		return "", "", false
	}
	return source, dest, true
}

// splitComposeVolumeParts splits docker compose short volume specs on ':' while
// ignoring colons that appear inside ${...} interpolations.
func splitComposeVolumeParts(spec string) []string {
	var parts []string
	var b strings.Builder
	depth := 0
	for i := 0; i < len(spec); i++ {
		c := spec[i]
		switch {
		case c == '$' && i+1 < len(spec) && spec[i+1] == '{':
			depth++
			b.WriteString("${")
			i++
		case c == '}' && depth > 0:
			depth--
			b.WriteByte(c)
		case c == ':' && depth == 0:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

func isAuthVolumeSource(source string) bool {
	lower := strings.ToLower(source)
	return strings.Contains(lower, "auth") ||
		strings.Contains(source, "CLI_PROXY_AUTH_PATH") ||
		strings.Contains(source, "${AUTH_PATH")
}

func isAuthVolumeDestination(dest string) bool {
	literal := normalizeAuthPathValue(dest)
	clean := filepath.Clean(literal)
	switch clean {
	case "/root/.cli-proxy-api", "/CLIProxyAPI/auths":
		return true
	default:
		return strings.HasSuffix(clean, "/auths") || strings.HasSuffix(clean, "/.cli-proxy-api")
	}
}

func normalizeAuthPathValue(value string) string {
	value = strings.TrimSpace(value)
	if literal, ok := composeDefaultLiteral(value); ok {
		return literal
	}
	return value
}

func composeDefaultLiteral(value string) (string, bool) {
	value = strings.TrimSpace(value)
	// ${VAR:-default} or ${VAR-default}
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		inner := value[2 : len(value)-1]
		if idx := strings.Index(inner, ":-"); idx >= 0 {
			return strings.TrimSpace(inner[idx+2:]), true
		}
		if idx := strings.Index(inner, "-"); idx >= 0 {
			return strings.TrimSpace(inner[idx+1:]), true
		}
	}
	return "", false
}

// ensureRuntimeEnvAuthPath pins AUTH_PATH in .env so entrypoint and compose
// interpolation share one destination after stack upgrades.
// force=true means preferredAuthPath comes from a concrete volume destination
// and must override a stale .env AUTH_PATH. force=false only fills a missing value.
func ensureRuntimeEnvAuthPath(lines *[]string, values map[string]string, preferredAuthPath string, force bool) {
	preferredAuthPath = strings.TrimSpace(preferredAuthPath)
	if preferredAuthPath == "" {
		preferredAuthPath = "/CLIProxyAPI/auths"
	}
	if force {
		setEnvValue(lines, values, "AUTH_PATH", preferredAuthPath)
		return
	}
	setEnvDefault(lines, values, "AUTH_PATH", preferredAuthPath)
}

func setEnvValue(lines *[]string, values map[string]string, key string, value string) {
	for i, line := range *lines {
		currentKey, _, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(currentKey) == key {
			(*lines)[i] = key + "=" + value
			values[key] = value
			return
		}
	}
	*lines = append(*lines, key+"="+value)
	values[key] = value
}

// preferredAuthPathFromCompose returns the AUTH_PATH that matches the target service
// auth volume destination. force is true only when the volume destination is a
// concrete path that must override a stale AUTH_PATH in .env.
func preferredAuthPathFromCompose(composeText string, service string) (path string, force bool) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(composeText), &doc); err != nil {
		return "", false
	}
	services, ok := stringMap(doc["services"])
	if !ok {
		return "", false
	}
	targetName := strings.TrimSpace(service)
	if _, ok := services[targetName]; !ok {
		targetName = firstApplicationService(services)
	}
	target, ok := stringMap(services[targetName])
	if !ok {
		return "", false
	}
	dest, destIsAuthPathDriven, found := findAuthVolumeDestination(target["volumes"])
	if found && !destIsAuthPathDriven && dest != "" {
		return dest, true
	}
	env := mergeEnv(target["environment"], nil)
	env, _ = alignAuthPathWithVolumes(env, target["volumes"])
	return normalizeAuthPathValue(stringValue(env["AUTH_PATH"])), false
}
