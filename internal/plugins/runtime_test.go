package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func writeProfileManifest(t *testing.T, profile, name, extra string) {
	t.Helper()
	codeDir := PluginCodeDir(profile, name)
	if err := os.MkdirAll(codeDir, 0755); err != nil {
		t.Fatal(err)
	}
	body := "name: " + name + "\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\n" + extra
	if err := os.WriteFile(filepath.Join(codeDir, "plugin.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLoadFromProfileRequiresEnv(t *testing.T) {
	profile := t.TempDir()
	if err := os.Chmod(profile, 0700); err != nil {
		t.Fatal(err)
	}
	writeProfileManifest(t, profile, "demo", "requires_env: [SYNTH_PLUGIN_TOKEN]\n")
	log := config.NewLogger(config.LevelWarn)
	m := NewManager(profile, "default", log)
	m.LoadFromProfile(profile)
	if got := m.EnabledNames(); len(got) != 0 {
		t.Fatalf("expected missing env to block load, got %v", got)
	}
	t.Setenv("SYNTH_PLUGIN_TOKEN", "synthetic")
	m2 := NewManager(profile, "default", log)
	m2.LoadFromProfile(profile)
	if got := m2.EnabledNames(); len(got) != 1 || got[0] != "demo" {
		t.Fatalf("expected demo enabled, got %v", got)
	}
}

func TestRuntimeRegisterIntoRegistry(t *testing.T) {
	log := config.NewLogger(config.LevelWarn)
	profile := t.TempDir()
	m := NewManager(profile, "default", log)
	def := llm.ToolDefinition{Name: "demo_echo", Description: "synthetic test tool"}
	def.Parameters.Type = "object"
	def.Parameters.Properties = map[string]llm.ToolParameterProperty{"q": {Type: "string"}}
	handler := func(ctx context.Context, args map[string]any) (string, error) { return "echo", nil }
	if _, err := m.RegisterTool("demo", "demo_echo", "core", def, handler, false, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(log.Server("tool.registry"))
	if err := m.RegisterIntoRegistry(reg, nil); err != nil {
		t.Fatal(err)
	}
	if !reg.HasHandler("demo_echo") {
		t.Fatal("expected handler registered")
	}
	got, err := reg.Execute(context.Background(), "demo_echo", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "echo" {
		t.Fatalf("got %q", got.Content)
	}
}
