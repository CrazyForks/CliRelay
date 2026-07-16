package usage

import (
	"database/sql"
	"testing"
)

func TestReplaceAllCcSwitchImportConfigsPersistsModelMappings(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()
	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}
	initCcSwitchImportConfigsTable(db)

	err := ReplaceAllCcSwitchImportConfigs([]CcSwitchImportConfigRow{
		{
			ID:                   "cfg-claude",
			ClientType:           "claude",
			ProviderName:         "Relay Claude",
			DefaultModel:         "kimi-k2.5",
			AllowedChannelGroups: []string{"kimicode"},
			EndpointPath:         "/v1",
			UsageAutoInterval:    30,
			APIKeyField:          "ANTHROPIC_API_KEY",
			ModelMappings: []CcSwitchModelMappingRow{
				{Role: "main", RequestModel: "kimi-k2.5", TargetModel: "kimi-k2.5"},
				{Role: "haiku", RequestModel: "claude-3-5-haiku", TargetModel: "kimi-k2.5", ContextWindow: 272000},
				{Role: "fable", RequestModel: "claude-fable-5", TargetModel: "kimi-k2.5"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs() error = %v", err)
	}

	rows := ListCcSwitchImportConfigs()
	if len(rows) != 1 {
		t.Fatalf("ListCcSwitchImportConfigs() length = %d, want 1: %#v", len(rows), rows)
	}
	if len(rows[0].ModelMappings) != 3 {
		t.Fatalf("model mappings length = %d, want 3: %#v", len(rows[0].ModelMappings), rows[0].ModelMappings)
	}
	if rows[0].ModelMappings[1].Role != "haiku" ||
		rows[0].ModelMappings[1].RequestModel != "claude-3-5-haiku" ||
		rows[0].ModelMappings[1].TargetModel != "kimi-k2.5" ||
		rows[0].ModelMappings[1].ContextWindow != 272000 {
		t.Fatalf("model mapping not preserved: %#v", rows[0].ModelMappings[1])
	}
	if rows[0].ModelMappings[2].Role != "fable" ||
		rows[0].ModelMappings[2].RequestModel != "claude-fable-5" ||
		rows[0].ModelMappings[2].TargetModel != "kimi-k2.5" {
		t.Fatalf("fable model mapping not preserved: %#v", rows[0].ModelMappings[2])
	}
}

func TestReplaceAllCcSwitchImportConfigsRejectsDuplicateRoutePaths(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()
	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}
	initCcSwitchImportConfigsTable(db)

	err := ReplaceAllCcSwitchImportConfigs([]CcSwitchImportConfigRow{
		{
			ID:                   "cfg-kimi-a",
			ClientType:           "claude",
			ProviderName:         "Kimi A",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"kimicode"},
			RoutePath:            "/kimicode/cs_same",
			EndpointPath:         "",
			UsageAutoInterval:    30,
		},
		{
			ID:                   "cfg-kimi-b",
			ClientType:           "claude",
			ProviderName:         "Kimi B",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"kimicode"},
			RoutePath:            "kimicode/cs_same/",
			EndpointPath:         "",
			UsageAutoInterval:    30,
		},
	})
	if err == nil {
		t.Fatal("ReplaceAllCcSwitchImportConfigs() error = nil, want duplicate route-path error")
	}
}

func TestFindCcSwitchImportConfigByRoutePath(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()
	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}
	initCcSwitchImportConfigsTable(db)

	if err := ReplaceAllCcSwitchImportConfigs([]CcSwitchImportConfigRow{
		{
			ID:                   "cfg-kimi-route",
			ClientType:           "claude",
			ProviderName:         "Kimi route",
			DefaultModel:         "kimi-k2.6",
			AllowedChannelGroups: []string{"kimicode"},
			RoutePath:            "/kimicode/cs_abcd1234",
			EndpointPath:         "",
			UsageAutoInterval:    30,
			ModelMappings: []CcSwitchModelMappingRow{
				{Role: "opus", RequestModel: "claude-opus-4-7", TargetModel: "kimi-k2.6"},
			},
		},
	}); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs() error = %v", err)
	}

	row, ok := FindCcSwitchImportConfigByRoutePath("https://relay.example.com/kimicode/cs_abcd1234/")
	if !ok {
		t.Fatal("FindCcSwitchImportConfigByRoutePath() ok = false, want true")
	}
	if row.ID != "cfg-kimi-route" {
		t.Fatalf("row.ID = %q, want %q", row.ID, "cfg-kimi-route")
	}
	if row.RoutePath != "/kimicode/cs_abcd1234" {
		t.Fatalf("row.RoutePath = %q, want normalized route path", row.RoutePath)
	}
	if len(row.ModelMappings) != 1 || row.ModelMappings[0].TargetModel != "kimi-k2.6" {
		t.Fatalf("row.ModelMappings = %#v, want kimi-k2.6 mapping", row.ModelMappings)
	}
}

func TestNormalizeCcSwitchModelMappingsClampsKnownCodexContext(t *testing.T) {
	mappings := normalizeCcSwitchModelMappings([]CcSwitchModelMappingRow{
		{RequestModel: "gpt-5.6-luna", TargetModel: "gpt-5.6-luna", ContextWindow: 2000000},
		{RequestModel: "unknown-model", TargetModel: "unknown-upstream", ContextWindow: 2000000},
	})
	if got := mappings[0].ContextWindow; got != 1050000 {
		t.Fatalf("known GPT-5.6 context = %d, want 1050000", got)
	}
	if got := mappings[1].ContextWindow; got != 2000000 {
		t.Fatalf("unknown model context = %d, want 2000000", got)
	}
}

func TestCcSwitchImportConfigsAreTenantScoped(t *testing.T) {
	initModelConfigTestDB(t)

	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	for tenantID, group := range map[string]string{tenantA: "group-a", tenantB: "group-b"} {
		if err := ReplaceAllCcSwitchImportConfigsForTenant(tenantID, []CcSwitchImportConfigRow{{
			ID:                   "shared-config",
			ClientType:           "codex",
			ProviderName:         group,
			DefaultModel:         "gpt-test",
			AllowedChannelGroups: []string{group},
			RoutePath:            "/shared-route",
		}}); err != nil {
			t.Fatalf("ReplaceAllCcSwitchImportConfigsForTenant(%s): %v", tenantID, err)
		}
	}

	for tenantID, group := range map[string]string{tenantA: "group-a", tenantB: "group-b"} {
		rows := ListCcSwitchImportConfigsForTenant(tenantID)
		if len(rows) != 1 || rows[0].ProviderName != group {
			t.Fatalf("tenant %s rows = %#v", tenantID, rows)
		}
		row, ok := FindCcSwitchImportConfigByRoutePathForTenant(tenantID, "/shared-route")
		if !ok || len(row.AllowedChannelGroups) != 1 || row.AllowedChannelGroups[0] != group {
			t.Fatalf("tenant %s route = %#v, ok=%v", tenantID, row, ok)
		}
	}
}

func TestCcSwitchImportConfigLegacySchemaMigratesToTenantPrimaryKey(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE ccswitch_import_configs (
		id TEXT PRIMARY KEY NOT NULL, client_type TEXT NOT NULL, provider_name TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '', default_model TEXT NOT NULL DEFAULT '', model_mappings TEXT NOT NULL DEFAULT '[]',
		allowed_channel_groups TEXT NOT NULL DEFAULT '[]', route_path TEXT NOT NULL DEFAULT '', endpoint_path TEXT NOT NULL DEFAULT '',
		usage_auto_interval INTEGER NOT NULL DEFAULT 30, api_key_field TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err = db.Exec(`INSERT INTO ccswitch_import_configs(id,client_type,provider_name,default_model) VALUES('legacy','codex','Legacy','gpt-test')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	initCcSwitchImportConfigsTable(db)

	pk, err := sqlitePrimaryKeyColumns(db, "ccswitch_import_configs")
	if err != nil {
		t.Fatalf("sqlitePrimaryKeyColumns: %v", err)
	}
	if len(pk) != 2 || pk[0] != "tenant_id" || pk[1] != "id" {
		t.Fatalf("primary key = %v", pk)
	}
	var tenantID, id string
	if err = db.QueryRow(`SELECT tenant_id,id FROM ccswitch_import_configs`).Scan(&tenantID, &id); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if tenantID != systemTenantID || id != "legacy" {
		t.Fatalf("migrated row = tenant %q id %q", tenantID, id)
	}
}
