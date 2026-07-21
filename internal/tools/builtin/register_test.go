package builtin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestRegisterDoesNotExposeSoulTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	for _, name := range reg.Names() {
		if strings.HasPrefix(name, "soul.") {
			t.Fatalf("soul tool exposed in registry: %q", name)
		}
	}
	for _, tool := range reg.LLMTools() {
		if strings.HasPrefix(tool.Function.Name, "soul.") {
			t.Fatalf("soul tool advertised to model: %q", tool.Function.Name)
		}
	}
}

func TestRegisterIncludesCurrentTimeTool(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	if !reg.HasHandler("time.current") {
		t.Fatal("time.current handler was not registered")
	}

	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != "time.current" {
			continue
		}
		if len(entry.Parameters) != 1 || entry.Parameters[0].Name != "timezone" || entry.Parameters[0].Type != "string" || !entry.Parameters[0].Required {
			t.Fatalf("unexpected time.current parameters: %+v", entry.Parameters)
		}
		return
	}
	t.Fatal("time.current schema was not loaded")
}

func TestRegisterIncludesTranscriptSearchTool(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	if !reg.HasHandler(toolnames.SessionTranscriptSearch) {
		t.Fatalf("%s handler was not registered", toolnames.SessionTranscriptSearch)
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != toolnames.SessionTranscriptSearch {
			continue
		}
		if len(entry.Parameters) != 2 || entry.Parameters[0].Name != "query" || entry.Parameters[0].Type != "string" || !entry.Parameters[0].Required || entry.Parameters[1].Name != "limit" || entry.Parameters[1].Type != "integer" || entry.Parameters[1].Required {
			t.Fatalf("unexpected %s parameters: %+v", toolnames.SessionTranscriptSearch, entry.Parameters)
		}
		return
	}
	t.Fatalf("%s schema was not loaded", toolnames.SessionTranscriptSearch)
}

func TestRegisterMemoryForgetUsesExactRequiredID(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != toolnames.UserMemoryForget {
			continue
		}
		if len(entry.Parameters) != 1 || entry.Parameters[0].Name != "memory_id" || entry.Parameters[0].Type != "integer" || !entry.Parameters[0].Required {
			t.Fatalf("unexpected %s parameters: %+v", toolnames.UserMemoryForget, entry.Parameters)
		}
		return
	}
	t.Fatalf("%s schema was not loaded", toolnames.UserMemoryForget)
}

func TestRegisterMemorySaveRequiresClaimIdentity(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != toolnames.UserMemorySave {
			continue
		}
		found := map[string]bool{}
		for _, parameter := range entry.Parameters {
			if parameter.Name == "claim_slot" || parameter.Name == "claim_value" {
				if !parameter.Required || parameter.Type != "string" {
					t.Fatalf("unexpected claim identity parameter: %+v", parameter)
				}
				found[parameter.Name] = true
			}
		}
		if !found["claim_slot"] || !found["claim_value"] {
			t.Fatalf("missing claim identity parameters: %+v", entry.Parameters)
		}
		return
	}
	t.Fatalf("%s schema was not loaded", toolnames.UserMemorySave)
}

func TestRegisterGlobalMemorySaveIsDefaultVisibleWithOptionalToolCallID(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	foundVisible := false
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name == toolnames.GlobalMemorySave {
			foundVisible = true
		}
	}
	if !foundVisible {
		t.Fatalf("%s is not default-visible", toolnames.GlobalMemorySave)
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != toolnames.GlobalMemorySave {
			continue
		}
		for _, parameter := range entry.Parameters {
			if parameter.Name == "source_tool_call_id" && parameter.Required {
				t.Fatal("source_tool_call_id must be optional")
			}
		}
		return
	}
	t.Fatalf("%s schema was not loaded", toolnames.GlobalMemorySave)
}

func TestRegisterDoesNotAdvertiseUnimplementedGlobalMemoryTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	advertised := map[string]bool{}
	for _, tool := range reg.LLMTools() {
		advertised[tool.Function.Name] = true
	}
	for _, name := range []string{toolnames.GlobalMemorySearch, toolnames.GlobalMemoryList, toolnames.GlobalMemoryForget} {
		if advertised[name] || reg.HasHandler(name) {
			t.Fatalf("unimplemented global memory tool is available: %s", name)
		}
	}
}

func TestRegisterAdvertisesFinalBuiltinToolNames(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"web.search":                      true,
		"time.current":                    true,
		toolnames.UserMemorySave:          true,
		toolnames.UserMemorySearch:        true,
		toolnames.UserMemoryList:          true,
		toolnames.UserMemoryForget:        true,
		toolnames.GlobalMemorySave:        true,
		toolnames.SessionTranscriptSearch: true,
	}
	got := map[string]bool{}
	for _, tool := range reg.LLMTools() {
		got[tool.Function.Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("advertised tools = %#v, want %#v", got, want)
	}
	for name := range want {
		if !got[name] || !reg.HasHandler(name) {
			t.Fatalf("final builtin tool is unavailable: %s", name)
		}
	}
}
