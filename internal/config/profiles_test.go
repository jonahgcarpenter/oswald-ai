package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const syntheticProfileConfig = `providers:
  fake:
    api: "http://localhost:9999/v1"
    key_env: PROFILE_TEST_MODEL_KEY
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: discord
      user_id: "123"
      profile: alice
    - platform: bluebubbles
      user_id: "+15551234567"
      profile: alice
platforms:
  api:
    enabled: true
    extra:
      api_host: "127.0.0.1"
      api_port: 12345
`

func profileConfigFixture(t *testing.T, text string) string {
	t.Helper()
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-process-credential")
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "api"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestProfileYAMLInheritanceAndNoDotenvLoading(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig)
	if err := os.WriteFile(filepath.Join(root, "profiles", "alice", "config.yaml"), []byte("model:\n  default: fake/override\n  context_length: 65536\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("PROFILE_TEST_MODEL_KEY=private-dotenv-canary\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMGatewayURL != "http://localhost:9999" || cfg.LLMGatewayAPIKey != "synthetic-process-credential" || cfg.Profiles["alice"].LLMGatewayModel != "fake/override" || cfg.Profiles["api"].LLMGatewayModel != "fake/model" || cfg.Profiles["alice"].ModelContextWindow != 65536 || len(cfg.ProfileRoutes) != 2 {
		t.Fatal("incorrect effective profile configuration")
	}
	if cfg.ProfileRoutes[1].Platform != "imessage" {
		t.Fatal("BlueBubbles route was not normalized")
	}
}

func TestProfileYAMLRejectsUnsafeOrUnsupportedConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"unknown field", syntheticProfileConfig + "unknown: private-yaml-canary\n"},
		{"non-loopback API", strings.ReplaceAll(syntheticProfileConfig, "127.0.0.1", "0.0.0.0")},
		{"traversal", strings.ReplaceAll(syntheticProfileConfig, "profile: alice", "profile: ../outside")},
		{"duplicate route", strings.ReplaceAll(syntheticProfileConfig, "platform: bluebubbles\n      user_id: \"+15551234567\"", "platform: discord\n      user_id: \"123\"")},
		{"duplicate YAML", syntheticProfileConfig + "model: {}\n"},
		{"multiple documents", syntheticProfileConfig + "---\nmodel: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := profileConfigFixture(t, tc.text)
			_, err := LoadProfiles(root)
			if err == nil {
				t.Fatal("invalid YAML accepted")
			}
			if strings.Contains(err.Error(), "private-yaml-canary") {
				t.Fatal("configuration content leaked")
			}
		})
	}
}

func TestProfileCannotOverrideGatewaysOrUseLocalCredentialFiles(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig)
	path := filepath.Join(root, "profiles", "alice", "config.yaml")
	if err := os.WriteFile(path, []byte("platforms: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfiles(root); err == nil {
		t.Fatal("profile listener override accepted")
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROFILE_TEST_MODEL_KEY", "")
	if err := os.WriteFile(filepath.Join(root, "profiles", "alice", ".env"), []byte("PROFILE_TEST_MODEL_KEY=synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfiles(root); err == nil {
		t.Fatal("missing process credential accepted")
	}
}
