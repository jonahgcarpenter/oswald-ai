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
    key: "${PROFILE_TEST_MODEL_KEY}"
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

func TestProfileRoutesWildcardCatchAll(t *testing.T) {
	text := `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
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
    - platform: discord
      user_id: "*"
      profile: guest
    - platform: "*"
      user_id: "*"
      profile: guest
platforms:
  api:
    enabled: true
    extra:
      api_host: "127.0.0.1"
      api_port: 12345
`
	root := profileConfigFixture(t, text)
	for _, name := range []string{"guest"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("wildcard routes rejected: %v", err)
	}
	found := map[string]string{}
	for _, route := range cfg.ProfileRoutes {
		found[route.Platform+":"+route.UserID] = route.Profile
	}
	if found["discord:*"] != "guest" || found["*:*"] != "guest" || found["discord:123"] != "alice" {
		t.Fatalf("wildcard routes not preserved: %v", found)
	}
}

func TestProfileRoutesWildcardRejected(t *testing.T) {
	routes := []string{
		// Cross-platform catch-all for one concrete user is ambiguous.
		`- platform: "*"
      user_id: "123"
      profile: alice`,
		// Duplicate global catch-all.
		`- platform: "*"
      user_id: "*"
      profile: guest
    - platform: "*"
      user_id: "*"
      profile: guest`,
		// Catch-all to an unknown profile.
		`- platform: discord
      user_id: "*"
      profile: ghost`,
	}
	for i, route := range routes {
		text := `providers:
  fake:
    api: "http://localhost:9999/v1"
    key: "${PROFILE_TEST_MODEL_KEY}"
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
gateway:
  multiplex_profiles: true
  profile_routes:
    ` + strings.ReplaceAll(route, "\n", "\n    ") + `
platforms:
  api:
    enabled: true
    extra:
      api_host: "127.0.0.1"
      api_port: 12345
`
		root := profileConfigFixture(t, text)
		if err := os.MkdirAll(filepath.Join(root, "profiles", "guest"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProfiles(root); err == nil {
			t.Fatalf("case %d: invalid wildcard route accepted", i)
		}
	}
}

func TestProfileRoutesWildcardNeedsMultiplex(t *testing.T) {
	text := strings.Replace(syntheticProfileConfig, "multiplex_profiles: true", "multiplex_profiles: false", 1)
	root := profileConfigFixture(t, text)
	if err := os.MkdirAll(filepath.Join(root, "profiles", "guest"), 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	withWildcard := strings.Replace(string(before), "    - platform: discord\n      user_id: \"123\"\n      profile: alice",
		"    - platform: discord\n      user_id: \"123\"\n      profile: alice\n    - platform: discord\n      user_id: \"*\"\n      profile: guest", 1)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(withWildcard), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfiles(root); err == nil {
		t.Fatal("non-default wildcard accepted without multiplexing")
	}
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
	if cfg.LLMGatewayURL != "http://localhost:9999" || cfg.LLMGatewayAPIKey != "synthetic-process-credential" || cfg.LLMGatewayProvider != "fake" || cfg.Profiles["alice"].LLMGatewayModel != "fake/override" || cfg.Profiles["alice"].ModelContextWindow != 65536 || len(cfg.ProfileRoutes) != 2 {
		t.Fatal("incorrect effective profile configuration")
	}
	if cfg.ProfileRoutes[1].Platform != "imessage" {
		t.Fatal("BlueBubbles route was not normalized")
	}
}

func TestProfileYAMLRejectsUnsafeOrUnsupportedConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"unknown field", syntheticProfileConfig + "unknown: private-yaml-canary\n"},
		{"non-loopback API", strings.ReplaceAll(syntheticProfileConfig, "127.0.0.1", "1.2.3.4")},
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
	// An explicitly empty process value overrides the profile's own dotenv.
	t.Setenv("PROFILE_TEST_MODEL_KEY", "")
	if err := os.WriteFile(filepath.Join(root, "profiles", "alice", ".env"), []byte("PROFILE_TEST_MODEL_KEY=synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfiles(root); err == nil {
		t.Fatal("missing process credential accepted")
	}
}
