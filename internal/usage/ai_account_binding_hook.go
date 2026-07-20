package usage

import (
	"context"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// AIAccountBindingHook owns only the low-frequency A-layer binding path. The
// per-request usage projection receives auth_subject_id directly from the
// selected auth and must never call this hook or query the binding table.
type AIAccountBindingHook struct {
	coreauth.NoopHook
}

func NewAIAccountBindingHook() coreauth.Hook { return AIAccountBindingHook{} }

func (AIAccountBindingHook) OnAuthRegistered(_ context.Context, auth *coreauth.Auth) {
	reconcileAIAccountBinding(auth)
}
func (AIAccountBindingHook) OnAuthUpdated(_ context.Context, auth *coreauth.Auth) {
	reconcileAIAccountBinding(auth)
}
func (AIAccountBindingHook) OnAuthLoaded(_ context.Context, auth *coreauth.Auth) {
	reconcileAIAccountBinding(auth)
}

func (AIAccountBindingHook) OnAuthDeleted(_ context.Context, auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if err := MarkAIAccountTenantBindingDeleted(auth.TenantID, auth.ID); err != nil {
		log.WithError(err).WithField("tenant_id", normalizeTenantID(auth.TenantID)).Warn("usage: soft-delete ai account binding")
	}
}

func reconcileAIAccountBinding(auth *coreauth.Auth) {
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		return
	}
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"tenant_id": normalizeTenantID(auth.TenantID), "provider": identity.Provider,
			"auth_subject_id": identity.ID,
		}).Warn("usage: reconcile ai account binding")
	}
}
