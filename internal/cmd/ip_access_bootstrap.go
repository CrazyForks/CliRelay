package cmd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/ipaccess"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// authEventRetention is how long authentication attempts are kept. Long enough
// to investigate an incident reported a few weeks late, short enough that a
// sustained attack cannot fill the disk.
const authEventRetention = 30 * 24 * time.Hour

// authEventPurgeInterval is how often the retention sweep runs.
const authEventPurgeInterval = 6 * time.Hour

// ipAccessPolicyStore adapts the runtime settings store to the persister
// interface the ipaccess package expects.
type ipAccessPolicyStore struct{}

func (ipAccessPolicyStore) Load() (ipaccess.ProtectionPolicy, bool) {
	payload, ok := settingsstore.GetRuntimeSettingPayload(ipaccess.SettingKey)
	if !ok || len(payload) == 0 {
		return ipaccess.ProtectionPolicy{}, false
	}
	var policy ipaccess.ProtectionPolicy
	if err := json.Unmarshal(payload, &policy); err != nil {
		log.WithError(err).Warn("ip-access: stored protection policy is unreadable, falling back to defaults")
		return ipaccess.ProtectionPolicy{}, false
	}
	return policy, true
}

func (ipAccessPolicyStore) Save(policy ipaccess.ProtectionPolicy) error {
	return settingsstore.UpsertRuntimeSetting(ipaccess.SettingKey, policy)
}

// initializeIPAccessControl wires the allow/deny list and the authentication
// attempt log into the process.
func initializeIPAccessControl(ctx context.Context, cfg *config.Config) {
	db := usage.RuntimeDB()
	if db == nil {
		return
	}
	var trustedProxies []string
	if cfg != nil {
		trustedProxies = cfg.TrustedProxies
	}
	// The registry and recorder themselves are installed by the same helpers the
	// management handler uses, so both startup paths converge on one instance.
	// What only the CLI runner can add is persistence: the settings store is off
	// limits to management handlers, and the retention sweep needs a long-lived
	// process to run in.
	recorder := authevents.EnsureDefault(ctx, db)
	startAuthEventRetention(ctx, recorder)

	registry := ipaccess.EnsureDefault(ctx, db, trustedProxies)
	registry.SetPolicyPersister(ctx, ipAccessPolicyStore{})
}

func startAuthEventRetention(ctx context.Context, recorder *authevents.Recorder) {
	if recorder == nil || !recorder.Available() {
		return
	}
	go func() {
		ticker := time.NewTicker(authEventPurgeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed, err := recorder.Purge(ctx, time.Now().Add(-authEventRetention))
				if err != nil {
					log.WithError(err).Debug("auth-events: retention sweep failed")
					continue
				}
				if removed > 0 {
					log.Debugf("auth-events: purged %d attempts older than %s", removed, authEventRetention)
				}
			}
		}
	}()
}
