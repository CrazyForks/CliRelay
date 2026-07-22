package contentmoderation

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "moderation.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := InitTables(db); err != nil {
		t.Fatalf("InitTables: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewStore(db)
}

func testProfile(t *testing.T, tenantID, name, mode, keywordMode string) Profile {
	t.Helper()
	profile, err := NewProfile(tenantID, uuid.NewString(), CreateProfileInput{
		Name:        name,
		Mode:        mode,
		KeywordMode: keywordMode,
		APIKey:      "moderation-secret",
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	return profile
}

func TestStoreTenantIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	profileA := testProfile(t, "tenant-a", "shared-name", ModeOff, KeywordModeAPIOnly)
	profileB := testProfile(t, "tenant-b", "shared-name", ModeOff, KeywordModeAPIOnly)
	profileB.ID = profileA.ID
	if err := store.CreateProfile(ctx, profileA); err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	if err := store.CreateProfile(ctx, profileB); err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
	got, err := store.GetProfile(ctx, "tenant-a", profileA.ID)
	if err != nil || got.TenantID != "tenant-a" {
		t.Fatalf("tenant A get = %#v, %v", got, err)
	}
	items, err := store.ListProfiles(ctx, "tenant-b")
	if err != nil || len(items) != 1 || items[0].TenantID != "tenant-b" {
		t.Fatalf("tenant B list = %#v, %v", items, err)
	}
}

func TestStoreDeleteProfileRestrictsBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	profile := testProfile(t, "tenant-a", "bound", ModeOff, KeywordModeAPIOnly)
	if err := store.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := profile.ID
	if err := store.PatchBindings(ctx, profile.TenantID, false, []BindingOperation{{
		ChannelType: ChannelTypeAuthFile,
		ChannelID:   "auth-1",
		ProfileID:   &profileID,
	}}); err != nil {
		t.Fatalf("PatchBindings: %v", err)
	}
	if err := store.DeleteProfile(ctx, profile.TenantID, profile.ID); !errors.Is(err, ErrProfileBound) {
		t.Fatalf("DeleteProfile error = %v, want ErrProfileBound", err)
	}
	if err := store.PatchBindings(ctx, profile.TenantID, false, []BindingOperation{{ChannelType: ChannelTypeAuthFile, ChannelID: "auth-1"}}); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if err := store.DeleteProfile(ctx, profile.TenantID, profile.ID); err != nil {
		t.Fatalf("DeleteProfile after unbind: %v", err)
	}
}

func TestStorePatchBindingsRequiresExplicitRebind(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first := testProfile(t, "tenant-a", "first", ModeOff, KeywordModeAPIOnly)
	second := testProfile(t, "tenant-a", "second", ModeOff, KeywordModeAPIOnly)
	for _, profile := range []Profile{first, second} {
		if err := store.CreateProfile(ctx, profile); err != nil {
			t.Fatalf("CreateProfile: %v", err)
		}
	}
	if err := store.PatchBindings(ctx, "tenant-a", false, []BindingOperation{{ChannelType: ChannelTypeProviderKey, ChannelID: "key-1", ProfileID: &first.ID}}); err != nil {
		t.Fatalf("initial bind: %v", err)
	}
	if err := store.PatchBindings(ctx, "tenant-a", false, []BindingOperation{{ChannelType: ChannelTypeProviderKey, ChannelID: "key-1", ProfileID: &second.ID}}); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("rebind error = %v, want conflict", err)
	}
	if err := store.PatchBindings(ctx, "tenant-a", true, []BindingOperation{{ChannelType: ChannelTypeProviderKey, ChannelID: "key-1", ProfileID: &second.ID}}); err != nil {
		t.Fatalf("allowed rebind: %v", err)
	}
	resolved, source, err := store.ResolveProfile(ctx, "tenant-a", "", "key-1", "")
	if err != nil || resolved.ID != second.ID || source != ChannelTypeProviderKey {
		t.Fatalf("ResolveProfile = %#v source=%q err=%v", resolved, source, err)
	}
}
