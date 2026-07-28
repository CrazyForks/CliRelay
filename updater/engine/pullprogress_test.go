package engine

import "testing"

// Real `docker compose pull` output captured without a TTY. The parser has to cope
// with the leading space, the ASCII bar, and mixed status casing.
const composePullSample = ` cli-proxy-api Pulling
 31e352740f53 Pulling fs layer
 8a1e25ce7c4f Pulling fs layer
 31e352740f53 Waiting
 31e352740f53 Downloading [>                                                  ]  310.4kB/30.43MB
 8a1e25ce7c4f Downloading [=====>                                             ]  1.05MB/10MB
 31e352740f53 Downloading [=========================>                         ]  15.2MB/30.43MB
 31e352740f53 Verifying Checksum
 31e352740f53 Download complete
 8a1e25ce7c4f Download complete
 31e352740f53 Extracting [==>                                                ]  1.8MB/30.43MB
 31e352740f53 Pull complete
 cli-proxy-api Pulled `

func TestPullTrackerAggregatesLayerBytes(t *testing.T) {
	tracker := NewPullTracker()
	for _, line := range splitLines(composePullSample) {
		tracker.Observe(line)
	}

	snapshot := tracker.Snapshot()
	if snapshot.Layers != 2 {
		t.Fatalf("Layers = %d, want 2", snapshot.Layers)
	}
	if snapshot.DoneLayers != 2 {
		t.Fatalf("DoneLayers = %d, want 2", snapshot.DoneLayers)
	}
	// Both layers completed, so the aggregate must sit at exactly 100 rather than
	// stalling below it because the last size line reported a partial count.
	if snapshot.Percent != 100 {
		t.Errorf("Percent = %v, want 100", snapshot.Percent)
	}
	wantTotal := int64(30.43*1000*1000) + int64(10*1000*1000)
	if snapshot.TotalBytes != wantTotal {
		t.Errorf("TotalBytes = %d, want %d", snapshot.TotalBytes, wantTotal)
	}
}

// TestPullTrackerIgnoresExtractionBytes pins the reason extraction is skipped:
// counting it would double the numerator and make the bar jump backwards.
func TestPullTrackerIgnoresExtractionBytes(t *testing.T) {
	tracker := NewPullTracker()
	tracker.Observe("31e352740f53 Downloading [==>   ]  5MB/10MB")
	midway := tracker.Snapshot()
	tracker.Observe("31e352740f53 Extracting [==>   ]  5MB/10MB")
	after := tracker.Snapshot()

	if after.Bytes != midway.Bytes {
		t.Errorf("extraction changed the byte count: %d -> %d", midway.Bytes, after.Bytes)
	}
	if after.Percent < midway.Percent {
		t.Errorf("progress went backwards: %v -> %v", midway.Percent, after.Percent)
	}
}

// TestPullTrackerExcludesCachedLayers checks that a layer already present locally
// does not drag the percentage down; it transfers nothing.
func TestPullTrackerExcludesCachedLayers(t *testing.T) {
	tracker := NewPullTracker()
	tracker.Observe("31e352740f53 Already exists")
	tracker.Observe("8a1e25ce7c4f Downloading [=====>  ]  5MB/10MB")

	snapshot := tracker.Snapshot()
	if snapshot.TotalBytes != 10*1000*1000 {
		t.Fatalf("TotalBytes = %d, want only the downloading layer", snapshot.TotalBytes)
	}
	if snapshot.Percent != 50 {
		t.Errorf("Percent = %v, want 50", snapshot.Percent)
	}
}

func TestPullTrackerNeverRegressesOnReorderedLines(t *testing.T) {
	tracker := NewPullTracker()
	tracker.Observe("31e352740f53 Downloading [========>  ]  9MB/10MB")
	high := tracker.Snapshot()
	// Docker occasionally reprints an earlier count; it must not undo progress.
	tracker.Observe("31e352740f53 Downloading [==>        ]  2MB/10MB")
	after := tracker.Snapshot()

	if after.Bytes != high.Bytes {
		t.Errorf("a stale count moved progress backwards: %d -> %d", high.Bytes, after.Bytes)
	}
}

func TestPullTrackerFallsBackToLayerCountsWithoutSizes(t *testing.T) {
	tracker := NewPullTracker()
	tracker.Observe("31e352740f53 Pulling fs layer")
	tracker.Observe("8a1e25ce7c4f Pulling fs layer")
	tracker.Observe("31e352740f53 Pull complete")

	snapshot := tracker.Snapshot()
	if snapshot.TotalBytes != 0 {
		t.Fatalf("TotalBytes = %d, want 0", snapshot.TotalBytes)
	}
	if snapshot.Percent != 50 {
		t.Errorf("Percent = %v, want 50 from layer counts", snapshot.Percent)
	}
}

func TestPullTrackerReportsNoEstimateBeforeAnyLayer(t *testing.T) {
	if percent := NewPullTracker().Snapshot().Percent; percent != -1 {
		t.Errorf("Percent = %v, want -1 to signal no estimate yet", percent)
	}
}

// TestPullTrackerDetectsSkippedPull guards a silent-failure path: compose exits 0
// when it skips a pull, which would otherwise let the update report success while
// still running the previous image.
func TestPullTrackerDetectsSkippedPull(t *testing.T) {
	tracker := NewPullTracker()
	tracker.Observe(" cli-proxy-api Skipped - Image is already being pulled by another service")

	message, skipped := tracker.Skipped()
	if !skipped {
		t.Fatal("a skipped pull must be detected")
	}
	if message == "" {
		t.Error("the skip reason should be retained for the failure message")
	}
}

func TestPullTrackerIgnoresNonLayerNoise(t *testing.T) {
	tracker := NewPullTracker()
	for _, line := range []string{
		"$ docker compose pull cli-proxy-api",
		"cli-proxy-api Pulling",
		"WARNING: something happened",
		"",
	} {
		tracker.Observe(line)
	}
	if snapshot := tracker.Snapshot(); snapshot.Layers != 0 {
		t.Errorf("noise was parsed as layers: %+v", snapshot)
	}
}

func TestParseDockerSize(t *testing.T) {
	cases := map[string]int64{
		"310.4kB": 310400,
		"30.43MB": 30430000,
		"1.5GB":   1500000000,
		"512B":    512,
		"":        0,
		"abc":     0,
	}
	for input, want := range cases {
		if got := parseDockerSize(input); got != want {
			t.Errorf("parseDockerSize(%q) = %d, want %d", input, got, want)
		}
	}
}

func splitLines(text string) []string {
	var lines []string
	start := 0
	for index := range len(text) {
		if text[index] == '\n' {
			lines = append(lines, text[start:index])
			start = index + 1
		}
	}
	return append(lines, text[start:])
}
