package cliproxy

import "testing"

func TestCatalogRefreshStateCoalescesConcurrentChanges(t *testing.T) {
	var state catalogRefreshState

	if !state.begin("tenant-a") {
		t.Fatal("first change should start a refresh")
	}
	// Edits landing while a refresh runs must not spawn a second upstream sweep.
	if state.begin("tenant-a") {
		t.Fatal("second change should not start a concurrent refresh")
	}
	// A different tenant is independent.
	if !state.begin("tenant-b") {
		t.Fatal("other tenant should refresh independently")
	}
	// ...but the coalesced change must still be picked up by one more pass.
	if !state.finish("tenant-a") {
		t.Fatal("pending change should require another pass")
	}
	if state.finish("tenant-a") {
		t.Fatal("no pending change left; refresh should stop")
	}
	if state.finish("tenant-b") {
		t.Fatal("tenant-b had no pending change")
	}
	// After finishing, a later change starts a fresh refresh.
	if !state.begin("tenant-a") {
		t.Fatal("change after completion should start a new refresh")
	}
}
