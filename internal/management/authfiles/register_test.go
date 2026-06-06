package authfiles

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRegistrarRegisterFileAddsAuthRecord(t *testing.T) {
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	path := filepath.Join(authDir, "claude-pro.json")
	data := []byte(`{"type":"claude","email":"pro@example.com","prefix":"team-a","proxy_url":"http://auth-proxy.example","proxy_id":"premium-egress"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := Registrar{Manager: manager, AuthDir: authDir}.RegisterFile(context.Background(), path, data)
	if err != nil {
		t.Fatalf("RegisterFile() error = %v", err)
	}
	auth, ok := manager.GetByID("claude-pro.json")
	if !ok || auth == nil {
		t.Fatal("registered auth not found")
	}
	if auth.Provider != "claude" || auth.Label != "pro@example.com" {
		t.Fatalf("provider/label = %q/%q", auth.Provider, auth.Label)
	}
	if auth.Prefix != "team-a" || auth.ProxyURL != "http://auth-proxy.example" || auth.ProxyID != "premium-egress" {
		t.Fatalf("routing fields = %q/%q/%q", auth.Prefix, auth.ProxyURL, auth.ProxyID)
	}
}

func TestRegistrarRegisterFileUpdatesExistingRelativeAuth(t *testing.T) {
	rootDir := t.TempDir()
	authDir := filepath.Join(rootDir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	previousWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(rootDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(previousWD)
	})

	manager := coreauth.NewManager(nil, nil, nil)
	fileName := "codex.json"
	path := filepath.Join(authDir, fileName)
	data := []byte(`{"type":"codex","email":"new@example.com"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	createdAt := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:        fileName,
		FileName:  fileName,
		Provider:  "codex",
		Label:     "old@example.com",
		Status:    coreauth.StatusActive,
		CreatedAt: createdAt,
		Attributes: map[string]string{
			"path": path,
		},
		Metadata: map[string]any{
			"type":  "codex",
			"email": "old@example.com",
		},
	}); err != nil {
		t.Fatalf("Register existing auth: %v", err)
	}

	err = Registrar{Manager: manager, AuthDir: "auths"}.RegisterFile(context.Background(), path, data)
	if err != nil {
		t.Fatalf("RegisterFile() error = %v", err)
	}
	auths := manager.List()
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.ID != fileName || auth.Label != "new@example.com" {
		t.Fatalf("updated auth = id %q label %q", auth.ID, auth.Label)
	}
	if !auth.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %v, want preserved %v", auth.CreatedAt, createdAt)
	}
}

func TestRegistrarRegisterFileNormalizesOpenAIBundle(t *testing.T) {
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	accountID := "acct-bundle"
	issuedAt := int64(1_779_210_280)
	expiresAt := int64(1_780_074_280)
	accessToken := makeRegisterJWT(t, map[string]any{
		"iat": issuedAt,
		"exp": expiresAt,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	idToken := makeRegisterJWT(t, map[string]any{
		"email": "bundle@example.com",
		"iat":   issuedAt,
		"exp":   expiresAt,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
	path := filepath.Join(authDir, "openai-bundle.json")
	data, err := json.Marshal(map[string]any{
		"version":              1,
		"platform":             "openai",
		"account_claims_email": "bundle@example.com",
		"access_token":         accessToken,
		"id_token":             idToken,
		"refresh_token":        "",
		"client_id":            "app_test",
		"chatgpt_account_id":   accountID,
		"disabled":             false,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err = Registrar{Manager: manager, AuthDir: authDir}.RegisterFile(context.Background(), path, data)
	if err != nil {
		t.Fatalf("RegisterFile() error = %v", err)
	}
	auth, ok := manager.GetByID("openai-bundle.json")
	if !ok || auth == nil {
		t.Fatal("registered auth not found")
	}
	if auth.Provider != "codex" || auth.Metadata["type"] != "codex" {
		t.Fatalf("provider/type = %q/%#v, want codex", auth.Provider, auth.Metadata["type"])
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("Unmarshal persisted: %v", err)
	}
	if persisted["account_id"] != accountID || persisted["type"] != "codex" {
		t.Fatalf("persisted normalized fields = %#v", persisted)
	}
}

func makeRegisterJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]any{"alg": "none", "typ": "JWT"}) + "." + encode(claims) + ".sig"
}
