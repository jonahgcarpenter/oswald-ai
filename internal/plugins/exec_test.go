package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func execFixture(t *testing.T, profile, name, body string) {
	t.Helper()
	dir := filepath.Join(PluginCodeDir(profile, name), "node_modules", ".bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestExecPluginBinary(t *testing.T) {
	profile := t.TempDir()
	execFixture(t, profile, "demo", "echo demo-output")
	if err := os.WriteFile(filepath.Join(PluginCodeDir(profile, "demo"), "plugin.yaml"), []byte("name: demo\n"), 0644); err != nil {
		if err := os.MkdirAll(PluginCodeDir(profile, "demo"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(PluginCodeDir(profile, "demo"), "plugin.yaml"), []byte("name: demo\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := ExecPluginBinary(t.Context(), profile, "demo", "demo", nil, 30, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(out) != "demo-output" {
		t.Fatalf("got %q", out)
	}
	if _, err := os.Stat(filepath.Join(profile, "plugins", "demo", "data", "home")); err != nil {
		t.Fatalf("plugin home missing: %v", err)
	}
	if _, err := ExecPluginBinary(t.Context(), profile, "demo", "missing", nil, 30, nil); err == nil {
		t.Fatal("missing binary accepted")
	}
	if _, err := ExecPluginBinary(t.Context(), profile, "demo", "../escape", nil, 30, nil); err == nil {
		t.Fatal("escape accepted")
	}
}

func TestExecPluginExitCode(t *testing.T) {
	profile := t.TempDir()
	execFixture(t, profile, "demo", "exit 3")
	if _, err := ExecPluginBinary(t.Context(), profile, "demo", "demo", nil, 30, config.NewLogger(config.LevelWarn)); err == nil {
		t.Fatal("nonzero exit accepted")
	} else if !strings.Contains(err.Error(), "code 3") {
		t.Fatalf("wrong error: %v", err)
	}
}
