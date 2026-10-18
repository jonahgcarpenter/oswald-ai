package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// disabledPlatformConfig disables BlueBubbles while leaving a required-looking
// credential reference unresolved. A disabled platform must load regardless.
const disabledPlatformConfig = `providers:
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
platforms:
  discord:
    enabled: true
    extra:
      token: "${PROFILE_TEST_DISCORD_TOKEN}"
  bluebubbles:
    enabled: false
    extra:
      server_url: http://192.168.4.206:1234
      server_password: ${OSWALD_TEST_DISABLED_SECRET}
      webhook_port: 8645
`

func disabledPlatformFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profiles", "alice"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(disabledPlatformConfig), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-model-key")
	t.Setenv("PROFILE_TEST_DISCORD_TOKEN", "synthetic-discord-token")
	return root
}

func TestDisabledPlatformCredentialsAreNeverRequired(t *testing.T) {
	root := disabledPlatformFixture(t)
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("disabled platform credential was resolved: %v", err)
	}
	if cfg.BlueBubblesListenPort != "" || cfg.BlueBubblesURL != "" || cfg.BlueBubblesPassword != "" {
		t.Fatal("disabled platform leaked into effective configuration")
	}
	if cfg.DiscordToken == "" {
		t.Fatal("enabled platform was not configured")
	}
}

func TestDisabledPlatformStillValidatesKeys(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"unknown extra key", "server_password"},
		{"unknown top key", "enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := disabledPlatformFixture(t)
			text := strings.Replace(disabledPlatformConfig, tc.field+":", tc.field+"_typo:", 1)
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadProfiles(root)
			configErr, ok := AsConfigError(err)
			if !ok || configErr.Code != "config_key_unknown" || !strings.Contains(configErr.Path, tc.field+"_typo") {
				t.Fatalf("disabled platform typo not reported: %v", err)
			}
		})
	}
	root := disabledPlatformFixture(t)
	text := strings.Replace(disabledPlatformConfig, "  bluebubbles:\n", "  telegram:\n", 1)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadProfiles(root)
	configErr, ok := AsConfigError(err)
	if !ok || configErr.Code != "config_platform_unsupported" {
		t.Fatalf("unsupported platform name accepted: %v", err)
	}
}

func TestPlatformEnablementFromUnsetVariableDisablesIt(t *testing.T) {
	root := disabledPlatformFixture(t)
	text := strings.Replace(disabledPlatformConfig, "enabled: false", `enabled: ${OSWALD_TEST_DISABLED_SECRET}`, 1)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil || cfg.DiscordToken == "" {
		t.Fatalf("unset enablement variable was not treated as disabled: %v", err)
	}
	// A disabled default keeps the platform off; unknown keys in its subtree are
	// still rejected because enablement is decided from raw YAML.
	text = strings.Replace(disabledPlatformConfig, "enabled: false", `enabled: ${OSWALD_TEST_DISABLED_SECRET:-false}`, 1)
	text = strings.Replace(text, "server_password:", "server_password_typo:", 1)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadProfiles(root)
	if configErr, ok := AsConfigError(err); !ok || configErr.Code != "config_key_unknown" {
		t.Fatalf("disabled platform typo was not reported: %v", err)
	}
	// A true default enables the platform, which then requires its credential.
	text = strings.Replace(disabledPlatformConfig, "enabled: false", `enabled: ${OSWALD_TEST_DISABLED_SECRET_YES:-true}`, 1)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadProfiles(root)
	if err == nil || ErrorCode(err) != "config_variable_missing" {
		t.Fatalf("enabled platform without credential was accepted: %v", err)
	}
	t.Setenv("OSWALD_TEST_DISABLED_SECRET", "synthetic-bluebubbles-password")
	if _, err := LoadProfiles(root); err != nil {
		t.Fatalf("enabled platform with credential failed: %v", err)
	}
}

func TestNamedProfilesInheritDefaultEnvironmentAndOverride(t *testing.T) {
	root := disabledPlatformFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OSWALD_TEST_INHERITED_MODEL_KEY=default-secret\nOSWALD_TEST_SHARED_KEY=default-shared\n"), 0600); err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(disabledPlatformConfig, "key: \"${PROFILE_TEST_MODEL_KEY}\"", `key: ${OSWALD_TEST_SHARED_KEY}`, 1)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMGatewayAPIKey != "default-shared" || cfg.Profiles["alice"].LLMGatewayAPIKey != "default-shared" {
		t.Fatal("inherited default credential was not available to a named profile")
	}
	// A named profile overrides the inherited value with its own dotenv.
	if err := os.WriteFile(filepath.Join(root, "profiles", "alice", ".env"), []byte("OSWALD_TEST_SHARED_KEY=alice-shared\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMGatewayAPIKey != "default-shared" || cfg.Profiles["alice"].LLMGatewayAPIKey != "alice-shared" {
		t.Fatal("named profile did not override inherited environment")
	}
	// The process environment still takes precedence over both.
	t.Setenv("OSWALD_TEST_SHARED_KEY", "process-shared")
	cfg, err = LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMGatewayAPIKey != "process-shared" || cfg.Profiles["alice"].LLMGatewayAPIKey != "process-shared" {
		t.Fatal("process environment did not take precedence over dotenv")
	}
}

func TestConfigErrorsCarrySafeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		text       string
		wantCode   string
		wantPath   string
		wantVar    string
		wantSource string
	}{
		{
			name:     "missing model credential",
			text:     strings.Replace(disabledPlatformConfig, "key: \"${PROFILE_TEST_MODEL_KEY}\"", `key: ${OSWALD_TEST_MISSING_MODEL_KEY}`, 1),
			wantCode: "config_variable_missing", wantPath: "providers.fake.key", wantVar: "OSWALD_TEST_MISSING_MODEL_KEY",
		},
		{
			name:     "missing brave credential",
			text:     disabledPlatformConfig + "tools:\n  web_search:\n    brave_key: ${OSWALD_TEST_MISSING_BRAVE_KEY}\n",
			wantCode: "config_variable_missing", wantPath: "tools.web_search.brave_key", wantVar: "OSWALD_TEST_MISSING_BRAVE_KEY",
		},
		{
			name:     "invalid model endpoint",
			text:     strings.Replace(disabledPlatformConfig, "http://localhost:9999/v1", "not a url", 1),
			wantCode: "config_model_endpoint_invalid", wantPath: "providers.fake.api",
		},
		{
			name:     "invalid bluebubbles endpoint",
			text:     strings.Replace(disabledPlatformConfig, "enabled: false", "enabled: true", 1),
			wantCode: "config_variable_missing", wantVar: "OSWALD_TEST_DISABLED_SECRET",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := disabledPlatformFixture(t)
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(tc.text), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadProfiles(root)
			configErr, ok := AsConfigError(err)
			if !ok {
				t.Fatalf("no classified error: %v", err)
			}
			if configErr.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", configErr.Code, tc.wantCode)
			}
			if tc.wantPath != "" && configErr.Path != tc.wantPath {
				t.Fatalf("path = %q, want %q", configErr.Path, tc.wantPath)
			}
			if tc.wantVar != "" && configErr.Variable != tc.wantVar {
				t.Fatalf("variable = %q, want %q", configErr.Variable, tc.wantVar)
			}
			fields := map[string]any{}
			for _, field := range configErr.LogFields() {
				fields[field.Key] = field.Value
			}
			if tc.wantPath != "" && fields["config_path"] != tc.wantPath {
				t.Fatalf("config_path field = %v", fields["config_path"])
			}
			if tc.wantVar != "" && fields["config_variable"] != tc.wantVar {
				t.Fatalf("config_variable field = %v", fields["config_variable"])
			}
			// Diagnostics never include the originating error text or a value.
			if strings.Contains(configErr.Error(), "synthetic") {
				t.Fatal("diagnostic leaked configuration content")
			}
		})
	}
}

func TestConfigErrorDiagnosticsAreEmittedSafely(t *testing.T) {
	root := disabledPlatformFixture(t)
	text := strings.Replace(disabledPlatformConfig, "key: \"${PROFILE_TEST_MODEL_KEY}\"", `key: "private-canary-${OSWALD_TEST_MISSING_MODEL_KEY}"`, 1)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadProfiles(root)
	configErr, ok := AsConfigError(err)
	if !ok {
		t.Fatalf("no classified error: %v", err)
	}
	if configErr.Code != "config_variable_missing" || configErr.Variable != "OSWALD_TEST_MISSING_MODEL_KEY" {
		t.Fatalf("unexpected diagnostics: %+v", configErr)
	}
	// The classification surfaces through the logging error code path.
	if ErrorCode(err) != "config_variable_missing" {
		t.Fatalf("error_code = %q", ErrorCode(err))
	}
	// A malformed variable name is omitted rather than logged.
	bad := &ConfigError{Code: "config_variable_missing", Variable: "not a name"}
	for _, field := range bad.LogFields() {
		if field.Key == "config_variable" {
			t.Fatal("unsafe variable name was logged")
		}
	}
	// Unknown artifact labels and path shapes are omitted.
	bad = &ConfigError{Code: "config_variable_missing", Source: "/etc/oswald", Path: "../../escape"}
	if len(bad.LogFields()) != 0 {
		t.Fatal("unsafe diagnostics were logged")
	}
}
