package config_manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
)

const testConfig = `providers:
  fake:
    api: http://localhost:9999/v1
    key: test-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 12345
  discord:
    enabled: true
    extra:
      token: discord-token
      allowed_users: ["111"]
      banned_users: []
runtime:
  log_level: info
`

func configContext() context.Context {
	return requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "default", Gateway: "discord", ExternalID: "111", Assurance: identity.AssuranceDiscordGateway})
}

func seedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(testConfig), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestHandlerRequiresPrincipal(t *testing.T) {
	handler := NewHandler(seedRoot(t), "default")
	if _, err := handler(context.Background(), map[string]interface{}{"action": "show"}); err == nil {
		t.Fatal("expected principal error")
	}
}

func TestHandlerRejectsUnknownArgs(t *testing.T) {
	handler := NewHandler(seedRoot(t), "default")
	if _, err := handler(configContext(), map[string]interface{}{"action": "show", "bogus": true}); err == nil {
		t.Fatal("expected unknown argument error")
	}
	if _, err := handler(configContext(), map[string]interface{}{"action": "dance"}); err == nil {
		t.Fatal("expected invalid action error")
	}
}

func TestShowAndGetRedactSecrets(t *testing.T) {
	handler := NewHandler(seedRoot(t), "default")
	shown, err := handler(configContext(), map[string]interface{}{"action": "show"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shown.Content, "test-key") {
		t.Fatal("secret leaked in show output")
	}
	got, err := handler(configContext(), map[string]interface{}{"action": "get", "path": "providers.fake.key"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Content, "test-key") || !strings.Contains(got.Content, "[redacted]") {
		t.Fatalf("secret not redacted: %q", got.Content)
	}
}

func TestInlineSetApplies(t *testing.T) {
	root := seedRoot(t)
	handler := NewHandler(root, "default")
	result, err := handler(configContext(), map[string]interface{}{"action": "set", "path": "runtime.log_level", "value": "debug"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "runtime.log_level") {
		t.Fatalf("unexpected confirmation %q", result.Content)
	}
	got, err := handler(configContext(), map[string]interface{}{"action": "get", "path": "runtime.log_level"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Content, "debug") {
		t.Fatalf("inline write did not apply: %q", got.Content)
	}
}

func TestSensitiveSetNeedsOperator(t *testing.T) {
	root := seedRoot(t)
	handler := NewHandler(root, "default")
	result, err := handler(configContext(), map[string]interface{}{"action": "set", "path": "providers.fake.key", "value": "new-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "operator approval") || !strings.Contains(result.Content, "oswald config set") {
		t.Fatalf("unexpected approval hint %q", result.Content)
	}
	if strings.Contains(result.Content, "new-secret") {
		t.Fatal("secret echoed in approval hint")
	}
	data, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if strings.Contains(string(data), "new-secret") {
		t.Fatal("sensitive write applied without approval")
	}
}

func TestNonSecretSensitiveSetCarriesCommand(t *testing.T) {
	root := seedRoot(t)
	handler := NewHandler(root, "default")
	result, err := handler(configContext(), map[string]interface{}{"action": "set", "path": "model.default", "value": "fake/other"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "oswald config set model.default fake/other") {
		t.Fatalf("operator command missing: %q", result.Content)
	}
}

func TestInvalidSetFailsWithoutHint(t *testing.T) {
	root := seedRoot(t)
	handler := NewHandler(root, "default")
	if _, err := handler(configContext(), map[string]interface{}{"action": "set", "path": "platforms.discord.extra.allowed_users", "value": `["!!!"]`}); err == nil {
		t.Fatal("expected validation error")
	}
	data, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if strings.Contains(string(data), "!!!") {
		t.Fatal("invalid write applied")
	}
}

func TestEnvShowListsNamesOnly(t *testing.T) {
	root := seedRoot(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TEST_KEY=secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(root, "default")
	result, err := handler(configContext(), map[string]interface{}{"action": "env_show"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "TEST_KEY") || strings.Contains(result.Content, "secret-value") {
		t.Fatalf("env names leaked values: %q", result.Content)
	}
}

func TestEnvSetNeedsOperator(t *testing.T) {
	root := seedRoot(t)
	handler := NewHandler(root, "default")
	result, err := handler(configContext(), map[string]interface{}{"action": "env_set", "name": "TEST_KEY", "value": "secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "oswald env set") || strings.Contains(result.Content, "secret-value") {
		t.Fatalf("unexpected env approval hint %q", result.Content)
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); !os.IsNotExist(err) {
		t.Fatal("env write applied without approval")
	}
}
