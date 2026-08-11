package authevents

import (
	"context"
	"database/sql"
	"sync"
)

// installed tracks which database the process-wide recorder was built for, so a
// swapped pool produces a recorder writing to the live handle rather than a
// closed one.
var installed struct {
	mu sync.Mutex
	db *sql.DB
}

// EnsureDefault installs the process-wide recorder for db and returns it.
//
// Idempotent, and bound to database availability rather than to one startup
// path — see the matching note in ipaccess.EnsureDefault for why that
// distinction was worth a bug.
func EnsureDefault(ctx context.Context, db *sql.DB) *Recorder {
	if db == nil {
		return Default()
	}
	installed.mu.Lock()
	defer installed.mu.Unlock()

	if existing := Default(); existing != nil && installed.db == db {
		return existing
	}
	if previous := Default(); previous != nil {
		previous.Close()
	}
	recorder := NewRecorder(db)
	recorder.Start(ctx)
	SetDefault(recorder)
	installed.db = db
	return recorder
}
