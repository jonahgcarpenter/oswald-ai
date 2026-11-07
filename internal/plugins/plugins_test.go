package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

func writeManifest(t *testing.T, dir, name, source string) {
	t.Helper()
	body := "name: " + name + "\nversion: \"1.0\"\ndescription: synthetic test plugin\n"
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	_ = source
}

func TestValidateManifestRejects(t *testing.T) {
	for _, name := range []string{"", "A", "soul", "a", "has space", "UPPER"} {
		m := &Manifest{Name: name, Source: "user"}
		if err := ValidateManifest(m); err == nil {
			t.Fatalf("expected reject for %q", name)
		}
	}
	m := &Manifest{Name: "ok-name", Source: "user", Capabilities: []string{"nope"}}
	if err := ValidateManifest(m); err == nil {
		t.Fatal("expected unknown capability reject")
	}
}

func TestDiscoveryPrecedenceAndImpostor(t *testing.T) {
	root := t.TempDir()
	bundled := filepath.Join(root, "bundled")
	user := filepath.Join(root, "user")
	writeManifest(t, filepath.Join(bundled, "kanban"), "kanban", "bundled")
	writeManifest(t, filepath.Join(user, "kanban"), "kanban", "user")
	// Impostor dir claiming the same manifest name as bundled but different dir.
	writeManifest(t, filepath.Join(user, "impostor_dir"), "kanban", "user")

	found := CollectManifests(bundled, user, "", false)
	if len(found) != 3 {
		t.Fatalf("got %d manifests", len(found))
	}
	winners := ResolveWinners(found)
	got, ok := winners["kanban"]
	if !ok {
		t.Fatal("missing kanban winner")
	}
	if got.Source != "user" {
		t.Fatalf("got source %q", got.Source)
	}
}

func TestManagerEnableToolOverrideGate(t *testing.T) {
	log := config.NewLogger(config.LevelWarn)
	m := NewManager(t.TempDir(), "default", log)
	m.Discover(map[string]Manifest{"demo": {Name: "demo", Key: "demo", Source: "user"}})
	if err := m.SetEnabled("demo", true); err != nil {
		t.Fatal(err)
	}
	def := llm.ToolDefinition{Name: "demo_tool", Description: "synthetic test tool"}
	def.Parameters.Type = "object"
	def.Parameters.Properties = map[string]llm.ToolParameterProperty{"q": {Type: "string"}}
	handler := func(ctx context.Context, args map[string]any) (string, error) { return "ok", nil }
	if _, err := m.RegisterTool("demo", "demo_tool", "core", def, handler, false, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RegisterTool("demo", "write_file", "core", def, handler, false, map[string]bool{"write_file": true}); err == nil {
		t.Fatal("expected override gate")
	}
	if _, err := m.RegisterTool("demo", "write_file", "core", def, handler, true, map[string]bool{"write_file": true}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerHookOrderAndTimeout(t *testing.T) {
	log := config.NewLogger(config.LevelWarn)
	m := NewManager(t.TempDir(), "default", log)
	var order []int
	_, _ = m.RegisterHook("a", HookPostToolCall, func(ctx context.Context, e map[string]any) error {
		order = append(order, 1)
		return nil
	})
	_, _ = m.RegisterHook("b", HookPostToolCall, func(ctx context.Context, e map[string]any) error {
		order = append(order, 2)
		return nil
	})
	if err := m.InvokeHook(context.Background(), HookPostToolCall, map[string]any{}, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("got order %v", order)
	}
	_, _ = m.RegisterHook("slow", HookPreToolCall, func(ctx context.Context, e map[string]any) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return nil
		}
	})
	if err := m.InvokeHook(context.Background(), HookPreToolCall, map[string]any{}, 20*time.Millisecond); err == nil {
		t.Fatal("expected timeout fail-closed")
	}
}

func TestManagerStorageConfined(t *testing.T) {
	log := config.NewLogger(config.LevelWarn)
	home := t.TempDir()
	m := NewManager(home, "default", log)
	root, err := m.StorageRoot("demo")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("bad perms %+v %v", info, err)
	}
	if _, err := m.StorageRoot("../escape"); err == nil {
		t.Fatal("expected escape reject")
	}
}
