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
	body := "name: " + name + "\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\n"
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
		m := &Manifest{Name: name, Source: "user", ManifestVersion: 2, ApiVersion: "1"}
		if err := ValidateManifest(m); err == nil {
			t.Fatalf("expected reject for %q", name)
		}
	}
	m := &Manifest{Name: "ok-name", Source: "user", ManifestVersion: 2, ApiVersion: "1", Capabilities: []string{"nope"}}
	if err := ValidateManifest(m); err == nil {
		t.Fatal("expected unknown capability reject")
	}
	for _, tc := range []Manifest{
		{Name: "demo", Source: "user", ApiVersion: "1"},
		{Name: "demo", Source: "user", ManifestVersion: 2},
		{Name: "demo", Source: "user", ManifestVersion: 1, ApiVersion: "1"},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "2"},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresPlugins: []string{"demo"}},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresPlugins: []string{"nope!"}},
	} {
		candidate := tc
		if err := ValidateManifest(&candidate); err == nil {
			t.Fatalf("expected contract reject for %+v", tc)
		}
	}
}

func TestResolveLoadOrderDependencies(t *testing.T) {
	winners := map[string]Manifest{
		"base":  {Name: "base", Key: "base", Source: "user", ManifestVersion: 2, ApiVersion: "1"},
		"mid":   {Name: "mid", Key: "mid", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresPlugins: []string{"base"}},
		"top":   {Name: "top", Key: "top", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresPlugins: []string{"mid"}},
		"other": {Name: "other", Key: "other", Source: "user", ManifestVersion: 2, ApiVersion: "1"},
	}
	ordered, err := ResolveLoadOrder(winners, []string{"top", "other"})
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	position := map[string]int{}
	for i, key := range ordered {
		position[key] = i
	}
	if !(position["base"] < position["mid"] && position["mid"] < position["top"]) {
		t.Fatalf("bad order %v", ordered)
	}
	if _, err := ResolveLoadOrder(winners, []string{"ghost"}); err == nil {
		t.Fatal("expected unknown reject")
	}
	cyclic := map[string]Manifest{
		"a": {Name: "a", Key: "a", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresPlugins: []string{"b"}},
		"b": {Name: "b", Key: "b", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresPlugins: []string{"a"}},
	}
	if _, err := ResolveLoadOrder(cyclic, []string{"a"}); err == nil {
		t.Fatal("expected cycle reject")
	}
	missing := map[string]Manifest{
		"a": {Name: "a", Key: "a", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresPlugins: []string{"ghost"}},
	}
	if _, err := ResolveLoadOrder(missing, []string{"a"}); err == nil {
		t.Fatal("expected missing dependency reject")
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

func TestEnabledRequiresEnvCoversEnabledOnly(t *testing.T) {
	log := config.NewLogger(config.LevelWarn)
	m := NewManager(t.TempDir(), "default", log)
	m.Discover(map[string]Manifest{
		"on":   {Name: "on", Key: "on", Source: "user", RequiresEnv: []string{"SYNTH_B_VAR", "SYNTH_A_VAR"}},
		"off":  {Name: "off", Key: "off", Source: "user", RequiresEnv: []string{"SYNTH_OFF_VAR"}},
		"bare": {Name: "bare", Key: "bare", Source: "user"},
	})
	if err := m.SetEnabled("on", true); err != nil {
		t.Fatal(err)
	}
	got := m.EnabledRequiresEnv()
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	names, ok := got["on"]
	if !ok || len(names) != 2 || names[0] != "SYNTH_B_VAR" {
		t.Fatalf("got %+v", got)
	}
	// Returned slices are copies: mutating them must not affect the manager.
	names[0] = "SYNTH_MUTATED"
	if again := m.EnabledRequiresEnv()["on"]; again[0] != "SYNTH_B_VAR" {
		t.Fatalf("manager state mutated: %+v", again)
	}
}
