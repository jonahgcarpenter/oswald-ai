package platforms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func TestChannelOverridesListEmpty(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "channel-overrides", "list")
	if code != 0 || !strings.Contains(out, "no channel overrides configured") {
		t.Fatalf("list empty = %d %q", code, out)
	}
}

func TestChannelOverridesAddListRemove(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, stderr := run(t, root, "channel-overrides", "add", "--platform", "discord", "--channel", "55501", "--provider", "fake", "--model", "fake/model"); code != 0 {
		t.Fatalf("add = %d %s", code, stderr)
	}
	code, out, stderr := run(t, root, "channel-overrides", "list", "--platform", "discord")
	if code != 0 || !strings.Contains(out, "55501") || !strings.Contains(out, "custom:fake") {
		t.Fatalf("list = %d %q %s", code, out, stderr)
	}
	// Upsert is idempotent: setting the same channel again replaces it.
	if code, _, stderr := run(t, root, "channel-overrides", "add", "--platform", "discord", "--channel", "55501", "--provider", "custom:fake", "--model", "fake/other"); code != 0 {
		t.Fatalf("re-add = %d %s", code, stderr)
	}
	if code, _, stderr := run(t, root, "channel-overrides", "remove", "--platform", "discord", "--channel", "55501"); code != 0 {
		t.Fatalf("remove = %d %s", code, stderr)
	}
	if code, _, _ := run(t, root, "channel-overrides", "remove", "--platform", "discord", "--channel", "55501"); code == 0 {
		t.Fatal("second remove of missing entry accepted")
	}
}

func TestChannelOverridesRejects(t *testing.T) {
	root := writeConfig(t, testConfig)
	for _, args := range [][]string{
		{"channel-overrides", "add", "--platform", "api", "--channel", "1", "--provider", "fake", "--model", "m"},
		{"channel-overrides", "add", "--platform", "discord", "--channel", "1", "--provider", "ghost", "--model", "m"},
		{"channel-overrides", "add", "--platform", "discord", "--channel", "1", "--provider", "fake"},
		{"channel-overrides", "add", "--platform", "discord", "--provider", "fake", "--model", "m"},
		{"channel-overrides", "remove", "--platform", "discord"},
		{"channel-overrides", "bogus"},
	} {
		if code, _, _ := run(t, root, args...); code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestChannelOverridesProfileOverlay(t *testing.T) {
	root := writeConfig(t, `providers:
  fake:
    api: "http://127.0.0.1:9999/v1"
  other:
    api: "http://127.0.0.1:9998/v1"
model:
  provider: custom:fake
  default: fake/model
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: discord
      user_id: "123"
      profile: alice
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
  discord:
    enabled: true
    extra:
      token: test-token
      banned_users: []
  bluebubbles:
    enabled: false
    extra:
      server_url: http://127.0.0.1:1234
      server_password: test-password
      webhook_port: 8090
`)
	aliceDir := filepath.Join(root, "profiles", "alice")
	if err := os.MkdirAll(aliceDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aliceDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, root, "channel-overrides", "add", "--platform", "discord", "--channel", "55501", "--provider", "other", "--model", "other/model", "--profile", "alice"); code != 0 {
		t.Fatalf("add overlay = %d %s", code, stderr)
	}
	cfg, err := config.Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	alice := cfg.Profiles["alice"]
	if alice == nil {
		t.Fatal("missing alice profile")
	}
	got, ok := alice.ChannelOverrides["discord"]["55501"]
	if !ok || got.Provider != "other" || got.Model != "other/model" {
		t.Fatalf("alice override = %+v ok=%v", got, ok)
	}
	if _, ok := cfg.ChannelOverrides["discord"]["55501"]; ok {
		t.Fatal("profile overlay leaked into the global map")
	}
	// A profile-private provider is usable in its own overlay.
	code, out, _ := run(t, root, "channel-overrides", "list", "--profile", "alice")
	if code != 0 || !strings.Contains(out, "55501") {
		t.Fatalf("list overlay = %d %q", code, out)
	}
}
