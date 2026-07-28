package engine

import (
	"strconv"
	"strings"
	"sync"
)

// PullTracker turns `docker compose pull` output into byte-level progress.
//
// The pull is by far the longest phase of an update — minutes on a small VPS — and
// previously reported as a single static step, so the panel's bar sat motionless at
// 20% and users concluded the update had hung. Docker already reports per-layer byte
// counts; this parses them back into an aggregate.
//
// The output is a stable, human-facing format rather than an API, so every field is
// treated as optional: an unparseable line advances nothing instead of failing the
// update. Layer counts are the fallback when byte totals are unavailable.
type PullTracker struct {
	mu     sync.Mutex
	layers map[string]*layerProgress

	// skipped records that compose declined to pull. Compose exits 0 in that case, so
	// without this an update can "succeed" while still running the old image.
	skipped    bool
	skippedLog string
}

type layerProgress struct {
	current  int64
	total    int64
	complete bool
	// exists marks a layer already present locally, which transfers no bytes and
	// must not drag the percentage down.
	exists bool
}

// PullSnapshot is an aggregate view of pull progress.
type PullSnapshot struct {
	Bytes      int64
	TotalBytes int64
	Layers     int
	DoneLayers int
	// Percent is -1 when there is not yet enough information to estimate one.
	Percent float64
}

func NewPullTracker() *PullTracker {
	return &PullTracker{layers: make(map[string]*layerProgress)}
}

// layerStatuses maps docker's per-layer status words to their effect. Docker prints
// these with varying capitalization across versions, so matching is case-folded.
var layerCompleteStatuses = map[string]bool{
	"download complete": true,
	"pull complete":     true,
	"already exists":    true,
}

// Observe feeds one line of pull output into the tracker.
func (t *PullTracker) Observe(line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Compose reports a skipped pull on the service line rather than a layer line.
	if strings.Contains(trimmed, "Skipped") {
		t.skipped = true
		if t.skippedLog == "" {
			t.skippedLog = trimmed
		}
		return
	}

	id, status, current, total, ok := parsePullLine(trimmed)
	if !ok {
		return
	}

	layer := t.layers[id]
	if layer == nil {
		layer = &layerProgress{}
		t.layers[id] = layer
	}

	lowered := strings.ToLower(status)
	if lowered == "already exists" {
		layer.exists = true
		layer.complete = true
		return
	}
	if layerCompleteStatuses[lowered] {
		layer.complete = true
		// A completed layer has transferred all of its bytes; without this the
		// aggregate stalls just below 100% because the final size line never arrives.
		if layer.total > 0 {
			layer.current = layer.total
		}
		return
	}
	// Only the download phase is counted. Extraction reports the same bytes a second
	// time, so including it would make the bar jump backwards when extraction starts.
	if lowered != "downloading" {
		return
	}
	if total > 0 {
		layer.total = total
	}
	if current > layer.current {
		layer.current = current
	}
}

// Snapshot returns the current aggregate.
func (t *PullTracker) Snapshot() PullSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	snapshot := PullSnapshot{Percent: -1}
	for _, layer := range t.layers {
		snapshot.Layers++
		if layer.complete {
			snapshot.DoneLayers++
		}
		if layer.exists {
			continue
		}
		snapshot.Bytes += layer.current
		snapshot.TotalBytes += layer.total
	}

	switch {
	case snapshot.TotalBytes > 0:
		percent := float64(snapshot.Bytes) * 100 / float64(snapshot.TotalBytes)
		if percent > 100 {
			percent = 100
		}
		snapshot.Percent = percent
	case snapshot.Layers > 0:
		snapshot.Percent = float64(snapshot.DoneLayers) * 100 / float64(snapshot.Layers)
	}
	return snapshot
}

// Skipped reports whether compose declined to pull, along with the line that said so.
func (t *PullTracker) Skipped() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.skippedLog, t.skipped
}

// parsePullLine extracts the layer id, status and any byte counts from a line of
// docker pull output. Recognized shapes:
//
//	31e352740f53 Downloading [===>    ]  310.4kB/30.43MB
//	31e352740f53 Download complete
//	31e352740f53 Already exists
//	 31e352740f53 Extracting [==>     ]  5.24MB/30.43MB
func parsePullLine(line string) (id string, status string, current int64, total int64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", 0, 0, false
	}

	id = strings.TrimSuffix(fields[0], ":")
	if !isLayerID(id) {
		return "", "", 0, 0, false
	}

	rest := strings.Join(fields[1:], " ")
	// Strip the ASCII progress bar; it carries no information the counts do not.
	if start := strings.Index(rest, "["); start >= 0 {
		if end := strings.Index(rest[start:], "]"); end >= 0 {
			rest = rest[:start] + rest[start+end+1:]
		}
	}

	statusFields := strings.Fields(rest)
	statusWords := make([]string, 0, len(statusFields))
	sizeToken := ""
	for _, field := range statusFields {
		if strings.Contains(field, "/") && strings.ContainsAny(field, "0123456789") {
			sizeToken = field
			continue
		}
		statusWords = append(statusWords, field)
	}
	status = strings.Join(statusWords, " ")
	if status == "" {
		return "", "", 0, 0, false
	}

	if sizeToken != "" {
		currentText, totalText, found := strings.Cut(sizeToken, "/")
		if found {
			current = parseDockerSize(currentText)
			total = parseDockerSize(totalText)
		}
	}
	return id, status, current, total, true
}

// isLayerID reports whether a token looks like a docker layer short id: a run of
// lowercase hex. Requiring a minimum length keeps ordinary words out.
func isLayerID(token string) bool {
	if len(token) < 8 || len(token) > 64 {
		return false
	}
	for _, r := range token {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// dockerSizeUnits are the suffixes docker prints. Docker uses decimal multiples for
// these labels, so kB is 1000 bytes rather than 1024.
var dockerSizeUnits = []struct {
	suffix     string
	multiplier int64
}{
	{"GB", 1000 * 1000 * 1000},
	{"MB", 1000 * 1000},
	{"kB", 1000},
	{"KB", 1000},
	{"B", 1},
}

func parseDockerSize(text string) int64 {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0
	}
	for _, unit := range dockerSizeUnits {
		if !strings.HasSuffix(trimmed, unit.suffix) {
			continue
		}
		number := strings.TrimSpace(strings.TrimSuffix(trimmed, unit.suffix))
		value, err := strconv.ParseFloat(number, 64)
		if err != nil {
			return 0
		}
		return int64(value * float64(unit.multiplier))
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0
	}
	return int64(value)
}
