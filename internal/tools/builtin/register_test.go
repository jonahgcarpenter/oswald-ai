package builtin

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestRegisterDoesNotExposeSoulTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, log); err != nil {
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
	if err := Register(reg, &config.Config{}, nil, nil, log); err != nil {
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
	if err := Register(reg, &config.Config{}, nil, nil, log); err != nil {
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

func TestRegisterExposesRetrievalOnlyUserMemoryTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{toolnames.UserMemorySearch, toolnames.UserMemoryList, toolnames.SessionTranscriptSearch} {
		if _, ok := reg.LLMTool(name); !ok || !reg.HasHandler(name) {
			t.Fatalf("retrieval tool is unavailable: %s", name)
		}
	}
}

func TestRegisterGlobalMemorySearchIsDefaultVisibleWithSchema(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	foundVisible := false
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name == toolnames.GlobalMemorySearch {
			foundVisible = true
		}
	}
	if !foundVisible {
		t.Fatalf("%s is not default-visible", toolnames.GlobalMemorySearch)
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != toolnames.GlobalMemorySearch {
			continue
		}
		if len(entry.Parameters) != 2 || entry.Parameters[0].Name != "query" || entry.Parameters[0].Type != "string" || !entry.Parameters[0].Required || entry.Parameters[1].Name != "limit" || entry.Parameters[1].Type != "integer" || entry.Parameters[1].Required {
			t.Fatalf("unexpected %s parameters: %+v", toolnames.GlobalMemorySearch, entry.Parameters)
		}
		return
	}
	t.Fatalf("%s schema was not loaded", toolnames.GlobalMemorySearch)
}

func TestRegisterCatalogOmitsRemovedGlobalMemoryTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	advertised := map[string]bool{}
	for _, tool := range reg.LLMTools() {
		advertised[tool.Function.Name] = true
	}
	cataloged := map[string]bool{}
	for _, entry := range reg.BuiltinCatalog() {
		cataloged[entry.Name] = true
	}
	for _, name := range []string{"global_memory_save", "global_memory_list", "global_memory_forget"} {
		if advertised[name] || cataloged[name] || reg.HasHandler(name) {
			t.Fatalf("removed global memory tool is available: %s", name)
		}
	}
}

func TestRegisterAdvertisesFinalBuiltinToolNames(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"web.search":                      true,
		"time.current":                    true,
		toolnames.UserMemorySearch:        true,
		toolnames.UserMemoryList:          true,
		toolnames.GlobalMemorySearch:      true,
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

func TestMemoryArgumentNormalizationPreservesInvalidLimitCorrection(t *testing.T) {
	normalize := normalizeMemorySearchArgs(8)
	invalid, err := governance.Fingerprint(toolnames.GlobalMemorySearch, map[string]interface{}{"query": "test", "limit": "8"}, normalize)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := governance.Fingerprint(toolnames.GlobalMemorySearch, map[string]interface{}{"query": "test", "limit": float64(8)}, normalize)
	if err != nil {
		t.Fatal(err)
	}
	if invalid == valid {
		t.Fatal("invalid limit and corrected valid limit produced the same fingerprint")
	}
}
