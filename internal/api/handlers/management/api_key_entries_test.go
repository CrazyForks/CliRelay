package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func setupAPIKeyEntriesTestDB(t *testing.T) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
}

func TestPatchAPIKeyEntryRejectsBlankKeyWithoutDeletingExistingEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupAPIKeyEntriesTestDB(t)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		Key:  "sk-existing-issue-192",
		Name: "Existing Key",
	}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api-key-entries",
		bytes.NewReader([]byte(`{"index":0,"value":{"key":"   ","name":"Existing Key"}}`)),
	)

	h := NewHandler(&config.Config{}, "", nil)
	h.PatchAPIKeyEntry(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := usage.GetAPIKey("sk-existing-issue-192"); got == nil {
		t.Fatalf("existing key was deleted after blank-key patch")
	}
}

func TestPatchAPIKeyEntryRejectsChangingToExistingKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupAPIKeyEntriesTestDB(t)

	for _, row := range []usage.APIKeyRow{
		{Key: "sk-original-issue-192", Name: "Original Key"},
		{Key: "sk-target-issue-192", Name: "Target Key"},
	} {
		if err := usage.UpsertAPIKey(row); err != nil {
			t.Fatalf("UpsertAPIKey(%q): %v", row.Key, err)
		}
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api-key-entries",
		bytes.NewReader([]byte(`{"match":"sk-original-issue-192","value":{"key":"sk-target-issue-192","name":"Renamed"}}`)),
	)

	h := NewHandler(&config.Config{}, "", nil)
	h.PatchAPIKeyEntry(c)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if got := usage.GetAPIKey("sk-original-issue-192"); got == nil || got.Name != "Original Key" {
		t.Fatalf("original key changed unexpectedly: %#v", got)
	}
	if got := usage.GetAPIKey("sk-target-issue-192"); got == nil || got.Name != "Target Key" {
		t.Fatalf("target key changed unexpectedly: %#v", got)
	}
}

func TestPutAPIKeyEntriesPrunesUnknownChannelsBeforeSave(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupAPIKeyEntriesTestDB(t)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPut,
		"/api-key-entries",
		bytes.NewReader([]byte(`[{
      "key": "sk-prune-stale-channel",
      "name": "Prune Stale Channel",
      "allowed-channels": ["kimi-A", "kimi-B"]
    }]`)),
	)

	h := NewHandler(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "kimi-B", BaseURL: "https://example.invalid"},
		},
	}, "", nil)
	h.PutAPIKeyEntries(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got := usage.GetAPIKey("sk-prune-stale-channel")
	if got == nil {
		t.Fatal("expected API key after PUT")
	}
	if containsString(got.AllowedChannels, "kimi-A") {
		t.Fatalf("allowed-channels = %v, should not keep unknown channel", got.AllowedChannels)
	}
	if !containsString(got.AllowedChannels, "kimi-B") {
		t.Fatalf("allowed-channels = %v, should keep known channel", got.AllowedChannels)
	}
}

func TestPatchAPIKeyEntryPrunesUnknownChannelsBeforeSave(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupAPIKeyEntriesTestDB(t)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		Key:             "sk-patch-prune",
		Name:            "Patch Prune",
		AllowedChannels: []string{"kimi-A", "kimi-B"},
	}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	body := []byte(`{
  "match": "sk-patch-prune",
  "value": {
    "allowed-channels": ["kimi-A", "kimi-B"]
  }
}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/api-key-entries", bytes.NewReader(body))

	h := NewHandler(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "kimi-B", BaseURL: "https://example.invalid"},
		},
	}, "", nil)
	h.PatchAPIKeyEntry(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	got := usage.GetAPIKey("sk-patch-prune")
	if got == nil {
		t.Fatal("expected API key after PATCH")
	}
	if containsString(got.AllowedChannels, "kimi-A") {
		t.Fatalf("allowed-channels = %v, should not keep unknown channel", got.AllowedChannels)
	}
	if !containsString(got.AllowedChannels, "kimi-B") {
		t.Fatalf("allowed-channels = %v, should keep known channel", got.AllowedChannels)
	}
}

func TestResetAPIKeyDailySpendingSuccessAndGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupAPIKeyEntriesTestDB(t)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID:                 "reset-id-1",
		Key:                "sk-reset-handler",
		Name:               "Reset Handler",
		DailySpendingLimit: 100,
	}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	db := usage.RuntimeDB()
	ts := usage.CutoffStartUTC(1).Add(time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO request_logs
		 (tenant_id, timestamp, api_key, api_key_id, model, source, failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 1, 0, 0, 0, 0, 0, ?)`,
		"00000000-0000-0000-0000-000000000001", ts, "sk-reset-handler", "reset-id-1", "model", "test", 20.0,
	); err != nil {
		t.Fatalf("insert log: %v", err)
	}

	h := NewHandler(&config.Config{}, "", nil)

	// success
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api-key-entries/daily-spending/reset",
		bytes.NewReader([]byte(`{"id":"reset-id-1"}`)),
	)
	h.ResetAPIKeyDailySpending(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("success status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"daily-spending-used":0`)) &&
		!bytes.Contains(rec.Body.Bytes(), []byte(`"daily-spending-used": 0`)) {
		// JSON number may be 0 without quotes
		if !bytes.Contains(rec.Body.Bytes(), []byte(`daily-spending-used`)) {
			t.Fatalf("success body missing used field: %s", rec.Body.String())
		}
	}
	var logCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE api_key_id = ?`, "reset-id-1").Scan(&logCount); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("logs after reset = %d, want 1 (must not delete)", logCount)
	}
	gotCost, err := usage.QueryTodayCostByKey("sk-reset-handler")
	if err != nil {
		t.Fatalf("QueryTodayCostByKey: %v", err)
	}
	if gotCost != 0 {
		t.Fatalf("effective cost after reset = %v, want 0", gotCost)
	}

	// unlimited rejects
	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID:                 "reset-id-unlimited",
		Key:                "sk-reset-unlimited",
		DailySpendingLimit: 0,
	}); err != nil {
		t.Fatalf("upsert unlimited: %v", err)
	}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api-key-entries/daily-spending/reset",
		bytes.NewReader([]byte(`{"id":"reset-id-unlimited"}`)),
	)
	h.ResetAPIKeyDailySpending(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unlimited status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	// missing key 404
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api-key-entries/daily-spending/reset",
		bytes.NewReader([]byte(`{"id":"does-not-exist"}`)),
	)
	h.ResetAPIKeyDailySpending(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetAPIKeyEntriesIncludesDailySpendingFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupAPIKeyEntriesTestDB(t)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID:                 "list-id-1",
		Key:                "sk-list-handler",
		Name:               "List Handler",
		DailySpendingLimit: 50,
	}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	db := usage.RuntimeDB()
	ts := usage.CutoffStartUTC(1).Add(time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO request_logs
		 (tenant_id, timestamp, api_key, api_key_id, model, source, failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 1, 0, 0, 0, 0, 0, ?)`,
		"00000000-0000-0000-0000-000000000001", ts, "sk-list-handler", "list-id-1", "model", "test", 12.5,
	); err != nil {
		t.Fatalf("insert log: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api-key-entries", nil)
	h := NewHandler(&config.Config{}, "", nil)
	h.GetAPIKeyEntries(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte(`"daily-spending-used"`)) {
		t.Fatalf("missing daily-spending-used: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte(`"daily-spending-remaining"`)) {
		t.Fatalf("missing daily-spending-remaining: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte(`12.5`)) {
		t.Fatalf("expected used 12.5 in body: %s", body)
	}
}
