package apikey

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestStoreTenantIsolation(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)

	tenantA := NewTenantStore(db, "00000000-0000-0000-0000-00000000000a")
	tenantB := NewTenantStore(db, "00000000-0000-0000-0000-00000000000b")
	if err := tenantA.Upsert(APIKeyRow{ID: "id-a", Key: "sk-a", Name: "A"}); err != nil {
		t.Fatalf("tenant A upsert: %v", err)
	}
	if got := tenantB.Get("sk-a"); got != nil {
		t.Fatalf("tenant B read tenant A key: %#v", got)
	}
	if err := tenantB.DeleteByID("id-a"); err != nil {
		t.Fatalf("tenant B delete by ID: %v", err)
	}
	if got := tenantA.GetByID("id-a"); got == nil {
		t.Fatal("tenant B delete removed tenant A key")
	}
	if err := tenantB.Upsert(APIKeyRow{ID: "id-b", Key: "sk-a", Name: "B"}); err == nil {
		t.Fatal("globally duplicate API key should be rejected")
	}
	if got := tenantB.List(); len(got) != 0 {
		t.Fatalf("tenant B list = %#v, want empty", got)
	}
}

func TestStoreAnyTenantLookupFindsGloballyUniqueKeyAndID(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)

	const tenantID = "00000000-0000-0000-0000-00000000000a"
	business := NewTenantStore(db, tenantID)
	if err := business.Upsert(APIKeyRow{ID: "business-id", Key: "sk-business", Name: "Business"}); err != nil {
		t.Fatalf("business upsert: %v", err)
	}

	system := NewStore(db)
	if got := system.Get("sk-business"); got != nil {
		t.Fatalf("tenant-scoped system lookup leaked business key: %#v", got)
	}
	if got := system.GetAnyTenant("sk-business"); got == nil || got.TenantID != tenantID || got.ID != "business-id" {
		t.Fatalf("GetAnyTenant = %#v, want business tenant row", got)
	}
	if got := system.GetByIDAnyTenant("business-id"); got == nil || got.TenantID != tenantID || got.Key != "sk-business" {
		t.Fatalf("GetByIDAnyTenant = %#v, want business tenant row", got)
	}
}

func TestReplaceAllPreservesOwnershipAndRejectsLastKeyDrop(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	InitTable(db)

	store := NewTenantStore(db, "00000000-0000-0000-0000-00000000000a")
	if err := store.Upsert(APIKeyRow{
		ID: "k1", Key: "sk-1", Name: "one", EndUserID: "eu-1", IsDefault: true,
	}); err != nil {
		t.Fatalf("seed k1: %v", err)
	}
	if err := store.Upsert(APIKeyRow{
		ID: "k2", Key: "sk-2", Name: "two", EndUserID: "eu-1", IsDefault: false,
	}); err != nil {
		t.Fatalf("seed k2: %v", err)
	}
	if err := store.Upsert(APIKeyRow{
		ID: "k3", Key: "sk-3", Name: "three", EndUserID: "eu-2", IsDefault: true,
	}); err != nil {
		t.Fatalf("seed k3: %v", err)
	}

	// Dropping eu-2's only key must fail (final ownership count, not id+key double-count).
	err = store.ReplaceAll([]APIKeyRow{
		{ID: "k1", Key: "sk-1", Name: "one-renamed"},
		{ID: "k2", Key: "sk-2", Name: "two"},
	})
	if err == nil {
		t.Fatal("expected last-key drop for eu-2 to fail")
	}

	// Client cannot reassign ownership on replace; keep both users' keys.
	if err := store.ReplaceAll([]APIKeyRow{
		{ID: "k1", Key: "sk-1", Name: "one-renamed", EndUserID: "hijack", IsDefault: false},
		{ID: "k2", Key: "sk-2", Name: "two", EndUserID: "hijack", IsDefault: true},
		{ID: "k3", Key: "sk-3", Name: "three", EndUserID: "hijack", IsDefault: false},
	}); err != nil {
		t.Fatalf("replace keep all: %v", err)
	}
	got1 := store.GetByID("k1")
	if got1 == nil || got1.EndUserID != "eu-1" || !got1.IsDefault || got1.Name != "one-renamed" {
		t.Fatalf("k1 after replace = %#v", got1)
	}
	got3 := store.GetByID("k3")
	if got3 == nil || got3.EndUserID != "eu-2" || !got3.IsDefault {
		t.Fatalf("k3 after replace = %#v", got3)
	}

	// id matches one owned key while key text matches another must not count as two kept owners.
	// Start from known state: eu-1 has k1+k2, eu-2 has k3 only.
	err = store.ReplaceAll([]APIKeyRow{
		// reuses id k1 (eu-1) but key text of k3 (eu-2) — only one row survives
		{ID: "k1", Key: "sk-3", Name: "confused"},
		{ID: "k2", Key: "sk-2", Name: "two"},
	})
	if err == nil {
		t.Fatal("expected replace that drops eu-2 via id/key confusion to fail")
	}
}
