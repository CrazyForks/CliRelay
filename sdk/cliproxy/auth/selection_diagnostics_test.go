package auth

import (
	"context"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// restrictedGroupManager reproduces the production shape that made this bug so hard
// to diagnose: a healthy credential in a channel group whose allowed-models list
// covers the chat model but not the image model.
func restrictedGroupManager(t *testing.T) *Manager {
	t.Helper()

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:          "default",
					AllowedModels: []string{"grok-4.5"},
				},
			},
		},
	})
	manager.RegisterExecutor(&stubExecutor{id: "xai"})

	if _, err := manager.Register(context.Background(), &Auth{
		ID:       "xai-auth",
		Label:    "Grok Account",
		Provider: "xai",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return manager
}

func pickWithModel(manager *Manager, model string) error {
	_, _, _, err := manager.pickNextMixed(
		context.Background(),
		[]string{"xai"},
		model,
		cliproxyexecutor.Options{},
		map[string]struct{}{},
	)
	return err
}

// TestBlockedModelReportsTheChannelGroup is the fix. Reporting "no auth available"
// for this case sent an operator hunting for a missing or broken account while the
// account was present and healthy, and cost several rounds of investigation before
// the allowed-models list was found.
func TestBlockedModelReportsTheChannelGroup(t *testing.T) {
	manager := restrictedGroupManager(t)

	err := pickWithModel(manager, "grok-imagine-image")
	if err == nil {
		t.Fatal("a model outside the group's allowed list must not be selectable")
	}

	var selectionErr *Error
	if asErr, ok := err.(*Error); ok {
		selectionErr = asErr
	} else {
		t.Fatalf("error type = %T, want *Error", err)
	}

	if selectionErr.Code != "model_not_allowed_by_channel_group" {
		t.Errorf("code = %q, want model_not_allowed_by_channel_group", selectionErr.Code)
	}
	// The message has to name both halves of the problem, because the fix is to
	// edit one specific list.
	if !strings.Contains(selectionErr.Message, "grok-imagine-image") {
		t.Errorf("message does not name the model: %q", selectionErr.Message)
	}
	if !strings.Contains(selectionErr.Message, "default") {
		t.Errorf("message does not name the channel group: %q", selectionErr.Message)
	}
}

// TestAllowedModelStillSelectable pins that the diagnosis did not change routing:
// a model on the list is still served.
func TestAllowedModelStillSelectable(t *testing.T) {
	manager := restrictedGroupManager(t)

	if err := pickWithModel(manager, "grok-4.5"); err != nil {
		t.Fatalf("an allowed model must still be selectable: %v", err)
	}
}

// TestGenericErrorRemainsWhenNoCredentialExists keeps the original message for the
// case it actually describes, so the new one stays meaningful.
func TestGenericErrorRemainsWhenNoCredentialExists(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{})
	manager.RegisterExecutor(&stubExecutor{id: "xai"})

	err := pickWithModel(manager, "grok-imagine-image")
	if err == nil {
		t.Fatal("expected an error when the tenant owns no credentials")
	}
	if asErr, ok := err.(*Error); ok && asErr.Code == "model_not_allowed_by_channel_group" {
		t.Error("a tenant with no credentials must not be told about channel groups")
	}
}

// TestUnrestrictedGroupDoesNotClaimBlocking guards against blaming a group that
// permits everything when the real cause is something else.
func TestUnrestrictedGroupDoesNotClaimBlocking(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{Name: "default"},
			},
		},
	})
	manager.RegisterExecutor(&stubExecutor{id: "xai"})
	if _, err := manager.Register(context.Background(), &Auth{
		ID: "xai-auth", Provider: "xai", Status: StatusActive,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := pickWithModel(manager, "grok-imagine-image"); err != nil {
		if asErr, ok := err.(*Error); ok && asErr.Code == "model_not_allowed_by_channel_group" {
			t.Errorf("a group without an allowed-models list must not be reported as blocking: %v", asErr.Message)
		}
	}
}
