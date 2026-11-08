package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func execFixture(t *testing.T, global, name, body string) {
	t.Helper()
	dir := filepath.Join(global, "plugins-available", name, "node_modules", ".bin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestExecPluginBinary(t *testing.T) {
	global := t.TempDir()
	profile := t.TempDir()
	execFixture(t, global, "demo", "echo demo-output")
	if err := os.WriteFile(filepath.Join(global, "plugins-available", "demo", "plugin.yaml"), []byte("name: demo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := ExecPluginBinary(t.Context(), global, profile, "demo", "demo", nil, 30, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(out) != "demo-output" {
		t.Fatalf("got %q", out)
	}
	if _, err := os.Stat(filepath.Join(profile, "plugins", "demo", "home")); err != nil {
		t.Fatalf("plugin home missing: %v", err)
	}
	if _, err := ExecPluginBinary(t.Context(), global, profile, "demo", "missing", nil, 30, nil); err == nil {
		t.Fatal("missing binary accepted")
	}
	if _, err := ExecPluginBinary(t.Context(), global, profile, "demo", "../escape", nil, 30, nil); err == nil {
		t.Fatal("escape accepted")
	}
}

func TestExecPluginExitCode(t *testing.T) {
	global := t.TempDir()
	profile := t.TempDir()
	execFixture(t, global, "demo", "exit 3")
	if _, err := ExecPluginBinary(t.Context(), global, profile, "demo", "demo", nil, 30, config.NewLogger(config.LevelWarn)); err == nil {
		t.Fatal("nonzero exit accepted")
	} else if !strings.Contains(err.Error(), "code 3") {
		t.Fatalf("wrong error: %v", err)
	}
}
