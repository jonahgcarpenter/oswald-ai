package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileEnvironmentInterpolationIsolationAndPrecedence(t *testing.T) {
	text := strings.Replace(syntheticProfileConfig, "key: \"${PROFILE_TEST_MODEL_KEY}\"", `key: "${OSWALD_TEST_PRIVATE_KEY}"`, 1)
	root := profileConfigFixture(t, text)
	for directory, value := range map[string]string{root: "default-secret", filepath.Join(root, "profiles", "alice"): "alice-secret", filepath.Join(root, "profiles", "api"): "api-secret"} {
		if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("OSWALD_TEST_PRIVATE_KEY="+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMGatewayAPIKey != "default-secret" || cfg.Profiles["alice"].LLMGatewayAPIKey != "alice-secret" {
		t.Fatal("profile environments were not isolated")
	}
	if _, exists := os.LookupEnv("OSWALD_TEST_PRIVATE_KEY"); exists {
		t.Fatal("dotenv mutated process environment")
	}
	t.Setenv("OSWALD_TEST_PRIVATE_KEY", "process-secret")
	cfg, err = LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMGatewayAPIKey != "process-secret" || cfg.Profiles["alice"].LLMGatewayAPIKey != "process-secret" {
		t.Fatal("process environment did not take precedence")
	}
}

func TestProfileInterpolationOccursAfterOverrides(t *testing.T) {
	root := profileConfigFixture(t, strings.Replace(syntheticProfileConfig, "key: \"${PROFILE_TEST_MODEL_KEY}\"", `key: "${OSWALD_TEST_OVERRIDE_KEY}"`, 1))
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("OSWALD_TEST_OVERRIDE_KEY=default-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"alice"} {
		if err := os.WriteFile(filepath.Join(root, "profiles", profile, "config.yaml"), []byte("providers:\n  fake:\n    key: replacement\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles["alice"].LLMGatewayAPIKey != "replacement" {
		t.Fatal("override was not applied before interpolation")
	}
}

func TestProfileInterpolationMissingVariableDoesNotLeakContent(t *testing.T) {
	root := profileConfigFixture(t, strings.Replace(syntheticProfileConfig, "key: \"${PROFILE_TEST_MODEL_KEY}\"", `key: "private-canary-${OSWALD_TEST_MISSING_KEY}"`, 1))
	_, err := LoadProfiles(root)
	if err == nil || strings.Contains(err.Error(), "private-canary") || strings.Contains(err.Error(), "OSWALD_TEST_MISSING_KEY") {
		t.Fatal("missing interpolation was accepted or disclosed private content")
	}
}

func TestProfileEnvironmentRejectsUnsafeFilesAndYAML(t *testing.T) {
	for _, mode := range []string{"symlink", "permissions", "duplicate", "alias", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			root := profileConfigFixture(t, syntheticProfileConfig)
			switch mode {
			case "symlink":
				if err := os.Symlink("config.yaml", filepath.Join(root, ".env")); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.WriteFile(filepath.Join(root, ".env"), []byte("KEY=private-canary\n"), 0644); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(syntheticProfileConfig+"model: {}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "alias":
				if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("providers: &loop\n  recursive: *loop\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(syntheticProfileConfig+"private_canary: unknown\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadProfiles(root); err == nil {
				t.Fatal("unsafe configuration was accepted")
			}
		})
	}
}

func TestEnvironmentInterpolationOperators(t *testing.T) {
	env := profileEnvironment{"SET": "value", "EMPTY": ""}
	for input, want := range map[string]string{"${SET}": "value", "${UNSET:-fallback}": "fallback", "${EMPTY:-fallback}": "fallback", "${EMPTY-fallback}": "", "$$${SET}": "$value"} {
		got, err := env.interpolate(input)
		if err != nil || got != want {
			t.Fatalf("interpolation result mismatch: %v", err)
		}
	}
	if _, err := env.interpolate("${UNSET:?private-canary}"); err == nil || strings.Contains(err.Error(), "private-canary") {
		t.Fatal("required interpolation did not fail safely")
	}
}

func TestProfileMCPConfigurationUsesProfileEnvironment(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig)
	if err := os.WriteFile(filepath.Join(root, "profiles", "alice", ".env"), []byte("OSWALD_TEST_MCP_TOKEN=alice-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profiles", "alice", "config.yaml"), []byte("mcp:\n  servers:\n    example:\n      url: https://example.com/mcp\n      description: Synthetic tools.\n      headers:\n        Authorization: Bearer ${OSWALD_TEST_MCP_TOKEN}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 0 || len(cfg.Profiles["alice"].MCPServers) != 1 {
		t.Fatal("MCP configuration escaped its profile")
	}
	if cfg.Profiles["alice"].MCPServers[0].Headers["Authorization"] != "Bearer alice-token" {
		t.Fatal("MCP credential was not interpolated")
	}
}
