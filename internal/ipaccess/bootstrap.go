package ipaccess

import (
	"context"
	"database/sql"
	"sync"
)

// installed tracks which database the process-wide registry was built for, so a
// runtime that swaps its pool (tests do this between cases, and a reconnect can
// do it in production) gets a registry pointing at the live handle instead of a
// closed one.
var installed struct {
	mu sync.Mutex
	db *sql.DB
}

// EnsureDefault installs the process-wide registry for db and returns it.
//
// It is idempotent and safe to call from every entry point. Binding the registry
// to "a database is available" rather than to one particular startup path is
// deliberate: the previous version initialised only from the CLI runner, so any
// caller that assembled the management API itself — the route smoke tests, and
// anyone embedding the server through the SDK — got a nil registry and every IP
// access endpoint answered 503.
func EnsureDefault(ctx context.Context, db *sql.DB, configuredProxies []string) *Registry {
	if db == nil {
		return Default()
	}
	installed.mu.Lock()
	defer installed.mu.Unlock()

	if existing := Default(); existing != nil && installed.db == db {
		existing.SetConfiguredProxies(configuredProxies)
		return existing
	}
	if previous := Default(); previous != nil {
		previous.Close()
	}
	registry := NewRegistry(NewStore(db))
	registry.SetConfiguredProxies(configuredProxies)
	registry.Start(ctx)
	SetDefault(registry)
	installed.db = db
	return registry
}
