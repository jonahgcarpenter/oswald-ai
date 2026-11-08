package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writePluginConfig(t *testing.T, root, body string) {
	t.Helper()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	minimal := syntheticProfileConfig
	full := minimal + body
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(full), 0600); err != nil {
		t.Fatal(err)
	}
}

func pluginFixture(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-process-credential")
	root := t.TempDir()
	for _, name := range []string{"alice", "api"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writePluginConfig(t, root, body)
	return root
}

func TestPluginsEnabledValidation(t *testing.T) {
	root := pluginFixture(t, "plugins:\n  enabled: [demo-one]\n  disabled: []\n")
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !cfg.PluginEnabledForProfile("demo-one") {
		t.Fatal("expected enabled")
	}
	if cfg.PluginEnabledForProfile("other") {
		t.Fatal("expected other disabled")
	}
}

func TestPluginsBothListsReject(t *testing.T) {
	root := pluginFixture(t, "plugins:\n  enabled: [demo]\n  disabled: [demo]\n")
	if _, err := Validate(root); err == nil {
		t.Fatal("expected both-lists reject")
	}
}

func TestPluginsEntriesValidation(t *testing.T) {
	root := pluginFixture(t, "plugins:\n  enabled: [demo]\n  entries:\n    demo:\n      allow_tool_override: true\n      mcp_allowlist: [files]\n      settings:\n        region: us\n")
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	entry, ok := cfg.PluginEntries["demo"]
	if !ok || !entry.AllowToolOverride {
		t.Fatalf("bad entry %+v", entry)
	}
	if len(entry.MCPAllowlist) != 1 || entry.MCPAllowlist[0] != "files" {
		t.Fatalf("bad allowlist %+v", entry.MCPAllowlist)
	}
}

func TestPluginsInvalidNameReject(t *testing.T) {
	root := pluginFixture(t, "plugins:\n  enabled: [Soul]\n")
	if _, err := Validate(root); err == nil {
		t.Fatal("expected invalid name reject")
	}
}
