package server

import (
	"encoding/json"
	"os"
	"testing"

	"clirelay.local/updater/protocol"
)

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeJSONFile(t *testing.T, path string, status protocol.Status) {
	t.Helper()
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		t.Fatalf("encode status: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write state file: %v", err)
	}
}
