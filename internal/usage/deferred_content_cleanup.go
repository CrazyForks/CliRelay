package usage

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

const staleDeferredUsageContentAge = 24 * time.Hour

var deferredUsageContentPatterns = []string{
	"cliproxy-usage-input-*",
	"cliproxy-usage-output-*",
	"cliproxy-usage-detail-*",
}

// CleanupStaleDeferredUsageContentFiles removes only closed spool files left by
// a previous process. Recent files are preserved so blue-green overlap cannot
// disrupt an in-flight request owned by the old instance.
func CleanupStaleDeferredUsageContentFiles() (int, error) {
	return cleanupStaleDeferredUsageContentFiles(os.TempDir(), time.Now().Add(-staleDeferredUsageContentAge))
}

func cleanupStaleDeferredUsageContentFiles(tempDir string, cutoff time.Time) (int, error) {
	seen := make(map[string]struct{})
	var errs []error
	removed := 0
	for _, pattern := range deferredUsageContentPatterns {
		matches, err := filepath.Glob(filepath.Join(tempDir, pattern))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, path := range matches {
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			info, errInfo := os.Lstat(path)
			if errInfo != nil {
				if !os.IsNotExist(errInfo) {
					errs = append(errs, errInfo)
				}
				continue
			}
			if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
				continue
			}
			if errRemove := os.Remove(path); errRemove != nil {
				if !os.IsNotExist(errRemove) {
					errs = append(errs, errRemove)
				}
				continue
			}
			removed++
		}
	}
	return removed, errors.Join(errs...)
}
