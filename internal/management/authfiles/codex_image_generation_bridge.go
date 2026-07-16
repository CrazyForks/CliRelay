package authfiles

import (
	"fmt"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const metadataKeyCodexImageGenerationBridge = "codex_image_generation_bridge"

// CodexImageGenerationBridgePayload returns the management-facing payload for
// per-account Codex /responses image_generation tool injection.
func CodexImageGenerationBridgePayload(auth *coreauth.Auth) map[string]any {
	if !isCodexOAuthAdmissionAuth(auth) {
		return nil
	}
	enabled, _ := auth.Metadata[metadataKeyCodexImageGenerationBridge].(bool)
	return map[string]any{
		"enabled": enabled,
	}
}

func ensureCodexImageGenerationBridgeEditable(auth *coreauth.Auth) error {
	if !isCodexOAuthAdmissionAuth(auth) {
		return fmt.Errorf("codex image generation bridge is only supported for Codex OAuth auth files")
	}
	return nil
}
