package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupStaleDeferredUsageContentFilesKeepsRecentAndUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	cutoff := now.Add(-time.Hour)

	write := func(name string, modTime time.Time) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		return path
	}

	oldPaths := []string{
		write("cliproxy-usage-input-old", old),
		write("cliproxy-usage-output-old", old),
		write("cliproxy-usage-detail-old", old),
	}
	recentPath := write("cliproxy-usage-output-recent", now)
	unrelatedPath := write("unrelated-old", old)

	removed, err := cleanupStaleDeferredUsageContentFiles(dir, cutoff)
	if err != nil {
		t.Fatalf("cleanupStaleDeferredUsageContentFiles() error = %v", err)
	}
	if removed != len(oldPaths) {
		t.Fatalf("removed = %d, want %d", removed, len(oldPaths))
	}
	for _, path := range oldPaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale deferred file still exists at %s: %v", path, err)
		}
	}
	for _, path := range []string{recentPath, unrelatedPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved file missing at %s: %v", path, err)
		}
	}
}
