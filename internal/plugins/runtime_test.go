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

func TestRuntimeLoadEnabledRequiresEnv(t *testing.T) {
	global := t.TempDir()
	profile := t.TempDir()
	if err := os.Chmod(global, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(profile, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(global, "plugins-available", "demo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := "name: demo\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\nrequires_env: [SYNTH_PLUGIN_TOKEN]\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	log := config.NewLogger(config.LevelWarn)
	m := NewManager(profile, "default", log)
	cfg := &config.Config{ProfileRoot: profile, ProfileName: "default", PluginEnabled: []string{"demo"}}
	m.LoadEnabledFromConfig(cfg, global)
	if got := m.EnabledNames(); len(got) != 0 {
		t.Fatalf("expected missing env to block load, got %v", got)
	}
	t.Setenv("SYNTH_PLUGIN_TOKEN", "synthetic")
	m2 := NewManager(profile, "default", log)
	m2.LoadEnabledFromConfig(cfg, global)
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
