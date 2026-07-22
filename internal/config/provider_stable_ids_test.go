package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

func TestLoadConfigBackfillsProviderStableIDsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `gemini-api-key:
  - api-key: gemini-secret
openai-compatibility:
  - name: compat
    base-url: https://compat.example
    api-key-entries:
      - api-key: compat-secret
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	first, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(first): %v", err)
	}
	ids := []string{
		first.GeminiKey[0].ID,
		first.OpenAICompatibility[0].ID,
		first.OpenAICompatibility[0].APIKeyEntries[0].ID,
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("backfilled ID %q is not UUID: %v", id, err)
		}
	}

	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(persisted, &raw); err != nil {
		t.Fatalf("parse migrated config: %v", err)
	}
	if len(persisted) == 0 {
		t.Fatal("migrated config is empty")
	}

	second, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(second): %v", err)
	}
	got := []string{
		second.GeminiKey[0].ID,
		second.OpenAICompatibility[0].ID,
		second.OpenAICompatibility[0].APIKeyEntries[0].ID,
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("ID changed on second load: first=%q second=%q", ids[i], got[i])
		}
	}
}

func TestEnsureProviderStableIDsRepairsDuplicates(t *testing.T) {
	duplicate := uuid.NewString()
	cfg := &Config{
		GeminiKey: []GeminiKey{{ID: duplicate, APIKey: "one"}},
		ClaudeKey: []ClaudeKey{{ID: duplicate, APIKey: "two"}},
	}
	if !cfg.EnsureProviderStableIDs() {
		t.Fatal("EnsureProviderStableIDs returned false for duplicate IDs")
	}
	if cfg.GeminiKey[0].ID == cfg.ClaudeKey[0].ID {
		t.Fatalf("duplicate IDs were not repaired: %q", duplicate)
	}
}
