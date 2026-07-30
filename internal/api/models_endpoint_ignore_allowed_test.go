package api

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestQueryFlagEnabled pins the flag parsing the channel-group editor relies on.
//
// Without it the editor's model list is filtered by the AllowedModels list it
// exists to edit, so a model that is not yet allowed can never be added — the
// control for fixing the restriction is gated on the restriction.
func TestQueryFlagEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	enabled := map[string]bool{
		"?ignore_group_allowed_models=1":    true,
		"?ignore_group_allowed_models=true": true,
		"?ignore_group_allowed_models=yes":  true,
		"?ignore_group_allowed_models=on":   true,
		"?ignore-group-allowed-models=1":    true,
		"?ignore_group_allowed_models=0":    false,
		"?ignore_group_allowed_models=":     false,
		"":                                  false,
	}

	for query, want := range enabled {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest("GET", "/models"+query, nil)
		got := queryFlagEnabled(ctx, "ignore_group_allowed_models", "ignore-group-allowed-models")
		if got != want {
			t.Errorf("query %q = %v, want %v", query, got, want)
		}
	}
}

// TestUnrelatedFlagsDoNotEnableIt guards the default: plaza and catalog omit the
// flag and must keep AllowedModels enforcement.
func TestUnrelatedFlagsDoNotEnableIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "/models?allowed_channel_groups=default", nil)

	if queryFlagEnabled(ctx, "ignore_group_allowed_models", "ignore-group-allowed-models") {
		t.Error("enforcement must stay on unless the editor explicitly asks for it")
	}
}
