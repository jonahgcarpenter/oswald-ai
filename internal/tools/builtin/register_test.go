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

func testConfig() *config.Config {
	return &config.Config{SearxngURL: "http://localhost:8080"}
}

func newTestRegistry(t *testing.T, log *config.Logger) *registry.Registry {
	t.Helper()
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRegisterDoesNotExposeSoulTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
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
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
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
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
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
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
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
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
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
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
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
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
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

func TestRegisterAdvertisesStrictWebSearchSchema(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name != "web.search" {
			continue
		}
		schema := tool.Function.Parameters
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties || len(schema.Properties) != 1 || len(schema.Required) != 1 || schema.Required[0] != "query" {
			t.Fatalf("web.search schema is not strict: %+v", schema)
		}
		return
	}
	t.Fatal("web.search schema was not advertised")
}

func TestRegisterRejectsInvalidSearxngURL(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{BraveAPIKey: "secret", SearxngURL: "localhost:8080"}, nil, nil, log); err == nil || !strings.Contains(err.Error(), "SearXNG web.search client") {
		t.Fatalf("invalid SearXNG URL registration error = %v", err)
	}
}

func TestRegisterWebSearchProviderMatrix(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *config.Config
		wantShown bool
	}{
		{name: "neither", cfg: &config.Config{}, wantShown: false},
		{name: "brave", cfg: &config.Config{BraveAPIKey: "secret"}, wantShown: true},
		{name: "searxng", cfg: &config.Config{SearxngURL: "http://localhost:8080"}, wantShown: true},
		{name: "both", cfg: &config.Config{BraveAPIKey: "secret", SearxngURL: "http://localhost:8080"}, wantShown: true},
		{name: "whitespace", cfg: &config.Config{BraveAPIKey: "  ", SearxngURL: "\t"}, wantShown: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := config.NewLogger(config.LevelError)
			reg := newTestRegistry(t, log)
			if err := Register(reg, test.cfg, nil, nil, log); err != nil {
				t.Fatal(err)
			}
			_, shown := reg.LLMTool("web.search")
			if shown != test.wantShown || reg.HasHandler("web.search") != test.wantShown {
				t.Fatalf("web.search shown=%t handler=%t", shown, reg.HasHandler("web.search"))
			}
			if test.wantShown {
				policy, ok := reg.Policy("web.search")
				if !ok || policy.History.Mode != governance.HistoryFull || !policy.History.SearchResult {
					t.Fatalf("web.search history policy = %+v", policy.History)
				}
			}
			reserved := false
			for _, name := range reg.Names() {
				reserved = reserved || name == "web.search"
			}
			if !reserved {
				t.Fatal("web.search name was not reserved")
			}
		})
	}
}

func TestRegisterLimitsWebSearchFailuresAndUnproductiveResults(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}

	for _, name := range reg.Names() {
		policy, ok := reg.Policy(name)
		if !ok {
			t.Fatalf("missing policy for %s", name)
		}
		if policy.MaxExecutions != 0 {
			t.Fatalf("%s has a per-tool execution limit: %+v", name, policy)
		}
		wantUnproductive := 0
		wantFailures := 0
		if name == "web.search" {
			wantUnproductive = 2
			wantFailures = 2
		}
		if policy.MaxUnproductive != wantUnproductive {
			t.Fatalf("%s max unproductive = %d, want %d", name, policy.MaxUnproductive, wantUnproductive)
		}
		if policy.MaxFailures != wantFailures {
			t.Fatalf("%s max failures = %d, want %d", name, policy.MaxFailures, wantFailures)
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
