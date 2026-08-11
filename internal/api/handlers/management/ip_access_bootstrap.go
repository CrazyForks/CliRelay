package management

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/ipaccess"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// ensureIPAccessRuntime binds the allow/deny list and the attempt log to
// whatever database the runtime currently holds.
//
// Both helpers are idempotent and re-bind when the pool is swapped, so calling
// this from handler construction and from every config reload is cheap and
// keeps the registry pointing at a live handle.
//
// Policy persistence is not wired here: it lives in the settings store, which
// management handlers deliberately do not reach into. The CLI runner attaches
// the persister on top. Without it the protection policy still applies, it just
// resets to defaults on restart — a far better failure mode than the endpoints
// answering 503.
func ensureIPAccessRuntime(cfg *config.Config) {
	db := usage.RuntimeDB()
	if db == nil {
		return
	}
	ctx := context.Background()
	authevents.EnsureDefault(ctx, db)

	var trustedProxies []string
	if cfg != nil {
		trustedProxies = cfg.TrustedProxies
	}
	ipaccess.EnsureDefault(ctx, db, ipaccess.ProxyTrustConfigured(trustedProxies))
}
