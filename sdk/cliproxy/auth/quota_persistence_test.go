package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

type quotaRuntimeStore struct {
	mu   sync.Mutex
	auth *Auth
}

func (s *quotaRuntimeStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth == nil {
		return nil, nil
	}
	return []*Auth{s.auth.Clone()}, nil
}

func (s *quotaRuntimeStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(auth.Metadata)
	if err != nil {
		return "", err
	}
	metadata := make(map[string]any)
	if err = json.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	s.auth = &Auth{
		ID:       auth.ID,
		TenantID: auth.TenantID,
		Provider: auth.Provider,
		Status:   StatusActive,
		Metadata: metadata,
	}
	return auth.ID, nil
}

func (s *quotaRuntimeStore) Delete(context.Context, string) error { return nil }

func TestManagerLoadRestoresWindowExhaustedGate(t *testing.T) {
	store := &quotaRuntimeStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "xai"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "grok-4.5",
		Error: &Error{
			Message:            `{"error":"Grok Build usage balance exhausted"}`,
			HTTPStatus:         http.StatusPaymentRequired,
			QuotaWindow:        "week",
			QuotaWindowMinutes: 10080,
		},
	})

	reloaded := NewManager(store, nil, nil)
	if err := reloaded.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, ok := reloaded.GetByID(auth.ID)
	if !ok || got == nil {
		t.Fatal("loaded auth missing")
	}
	if !got.Quota.Exceeded || !got.Quota.RecoveryRequired {
		t.Fatalf("loaded quota = %#v, want confirmed-recovery gate", got.Quota)
	}
	blocked, _, _ := isAuthBlockedForModel(got, "grok-4.5", got.UpdatedAt)
	if !blocked {
		t.Fatal("loaded week-exhausted auth is selectable")
	}
}
