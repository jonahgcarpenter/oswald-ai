package tools

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	toolmemory "github.com/jonahgcarpenter/oswald-ai/internal/tools/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/transcriptsearch"
	websearch "github.com/jonahgcarpenter/oswald-ai/internal/tools/web_search"
)

func testConfig() *config.Config {
	return &config.Config{SearxngURL: "http://localhost:8080", ComfyUICheckpoint: "dreamshaper_8.safetensors"}
}

func visibleTestTool(reg *registry.Registry, name string) (llm.Tool, bool) {
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name == name {
			return tool, true
		}
	}
	return llm.Tool{}, false
}

func newTestRegistry(t *testing.T, cfg *config.Config) *registry.Registry {
	t.Helper()
	reg, err := NewRegistryFromConfig(cfg, nil, nil, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRegisterDoesNotExposeSoulTools(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
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

func TestRegisterIncludesFileMemoryTool(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
	if !reg.HasHandler(toolmemory.Name) {
		t.Fatal("memory handler was not registered")
	}
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name != toolmemory.Name {
			continue
		}
		params := tool.Function.Parameters
		if len(params.Properties) != 6 || params.Properties["target"].Type != "string" || params.Properties["operations"].Type != "array" || len(params.Required) != 1 || params.Required[0] != "target" {
			t.Fatalf("unexpected memory parameters: %+v", params)
		}
		return
	}
	t.Fatal("memory schema was not loaded")
}

func TestRegisterHidesTranscriptSearch(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
	if _, ok := visibleTestTool(reg, transcriptsearch.Name); ok || reg.HasHandler(transcriptsearch.Name) {
		t.Fatal("transcript search is available")
	}
}

func TestRegisterCatalogOmitsRemovedMemoryTools(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
	advertised := map[string]bool{}
	for _, tool := range reg.LLMTools() {
		advertised[tool.Function.Name] = true
	}
	cataloged := map[string]bool{}
	for _, name := range reg.Names() {
		cataloged[name] = true
	}
	for _, name := range []string{"user_memory_save", "user_memory_search", "user_memory_list", "global_memory_search", "global_memory_save", "global_memory_list", "global_memory_forget"} {
		if advertised[name] || cataloged[name] || reg.HasHandler(name) {
			t.Fatalf("removed memory tool is available: %s", name)
		}
	}
}

func TestRegisterAdvertisesFinalBuiltinToolNames(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
	want := map[string]bool{
		websearch.Name:  true,
		toolmemory.Name: true,
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

func TestRegisterMemoryPolicyAndBatchSchema(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
	policy, ok := reg.Policy(toolmemory.Name)
	if !ok || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult {
		t.Fatalf("unexpected file memory policy: %+v", policy)
	}
	tool, ok := visibleTestTool(reg, toolmemory.Name)
	if !ok {
		t.Fatal("memory schema is unavailable")
	}
	operations := tool.Function.Parameters.Properties["operations"]
	if tool.Function.Parameters.AdditionalProperties != nil || operations.MinItems != nil || operations.MaxItems != nil || operations.Items == nil || operations.Items.AdditionalProperties != nil {
		t.Fatalf("memory schema differs from advertised batch shape: %+v", tool.Function.Parameters)
	}
	for _, name := range []string{"action", "content", "new_text", "old_text"} {
		if _, ok := operations.Items.Properties[name]; !ok {
			t.Fatalf("memory schema is missing %s", name)
		}
	}
}

func TestRegisterAdvertisesStrictWebSchemas(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
	wantParameter := map[string]string{websearch.Name: "query"}
	for _, tool := range reg.LLMTools() {
		parameter, exists := wantParameter[tool.Function.Name]
		if !exists {
			continue
		}
		schema := tool.Function.Parameters
		limit := schema.Properties["limit"]
		if limit.Type != "integer" || limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != websearch.MaxWebResults || limit.Default != websearch.DefaultWebResults || limit.Description != "Maximum number of results to return. Defaults to 5." {
			t.Fatalf("web_search limit schema = %+v", limit)
		}
		wire, err := json.Marshal(limit)
		if err != nil || !strings.Contains(string(wire), `"default":5`) {
			t.Fatalf("web_search limit default missing from wire schema: %s, %v", wire, err)
		}
		query := schema.Properties["query"]
		if query.Type != "string" || query.Description != `The search query to look up on the web. You may include backend-supported operators such as site:example.com, filetype:pdf, intitle:word, -term, or "exact phrase".` || tool.Function.Description != `Search the web for information. Returns up to 5 results by default with titles, URLs, and descriptions. The query is passed through to the configured backend, so operators such as site:domain, filetype:pdf, intitle:word, -term, and "exact phrase" may work when the backend supports them.` {
			t.Fatalf("web_search query/description = %+v / %q", query, tool.Function.Description)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties || len(schema.Properties) != 2 || len(schema.Required) != 1 || schema.Required[0] != parameter {
			t.Fatalf("%s schema is not strict: %+v", tool.Function.Name, schema)
		}
		delete(wantParameter, tool.Function.Name)
	}
	if len(wantParameter) != 0 {
		t.Fatalf("web schemas were not advertised: %v", wantParameter)
	}
}

func TestSearchFingerprintsIncludeEffectiveResultLimit(t *testing.T) {
	reg := newTestRegistry(t, testConfig())
	const parameter = "limit"
	name := websearch.Name
	defaultLimit, maxLimit := websearch.DefaultWebResults, websearch.MaxWebResults
	policy, ok := reg.Policy(name)
	if !ok || !policy.BlockDuplicates || policy.NormalizeArgs == nil {
		t.Fatal("missing duplicate policy")
	}
	omitted, err := governance.Fingerprint(name, map[string]interface{}{"query": " Search   Query "}, policy.NormalizeArgs)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []interface{}{defaultLimit, float64(defaultLimit), int64(defaultLimit), json.Number(strconv.Itoa(defaultLimit))} {
		got, err := governance.Fingerprint(name, map[string]interface{}{"query": "search query", parameter: raw}, policy.NormalizeArgs)
		if err != nil || got != omitted {
			t.Fatalf("default %v differs: %v", raw, err)
		}
	}
	seen := map[string]bool{}
	for limit := 1; limit <= maxLimit; limit++ {
		got, err := governance.Fingerprint(name, map[string]interface{}{"query": "search query", parameter: float64(limit)}, policy.NormalizeArgs)
		if err != nil || seen[got] {
			t.Fatalf("limit %d not distinct: %v", limit, err)
		}
		seen[got] = true
	}
	for _, raw := range []interface{}{nil, "2", 0, -1, 1.5, maxLimit + 1, math.Inf(1)} {
		args := map[string]interface{}{"query": "search query", parameter: raw}
		normalized := policy.NormalizeArgs(args).(map[string]interface{})
		if !reflect.DeepEqual(normalized[parameter], map[string]interface{}{"invalid": raw}) {
			t.Fatal("invalid value not preserved")
		}
		got, err := governance.Fingerprint(name, args, policy.NormalizeArgs)
		if err == nil && got == omitted {
			t.Fatal("invalid value collides with default")
		}
	}
	legacy, err := governance.Fingerprint(name, map[string]interface{}{"query": "search query", "results": 2}, policy.NormalizeArgs)
	if err != nil || legacy != omitted {
		t.Fatalf("unrecognized results field changed fingerprint: %v", err)
	}
}

func TestRegisterRejectsInvalidSearxngURL(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	if _, err := NewRegistryFromConfig(&config.Config{BraveAPIKey: "secret", SearxngURL: "localhost:8080"}, nil, nil, log); err == nil || !strings.Contains(err.Error(), "SearXNG web_search client") {
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
			reg := newTestRegistry(t, test.cfg)
			for _, toolName := range []string{websearch.Name} {
				_, shown := visibleTestTool(reg, toolName)
				if shown != test.wantShown || reg.HasHandler(toolName) != test.wantShown {
					t.Fatalf("%s shown=%t handler=%t", toolName, shown, reg.HasHandler(toolName))
				}
				if test.wantShown {
					policy, ok := reg.Policy(toolName)
					if !ok {
						t.Fatalf("missing %s policy", toolName)
					}
					if policy.History.Mode != governance.HistoryFull || !policy.History.SearchResult {
						t.Fatalf("web_search history policy = %+v", policy.History)
					}
				}
				reserved := false
				for _, name := range reg.Names() {
					reserved = reserved || name == toolName
				}
				if !reserved {
					t.Fatalf("%s name was not reserved", toolName)
				}
			}
			for _, removed := range []string{"web.fetch", "web.image_search", "web.image_select"} {
				if _, shown := visibleTestTool(reg, removed); shown || reg.HasHandler(removed) {
					t.Fatalf("removed tool is available: %s", removed)
				}
				for _, name := range reg.Names() {
					if name == removed {
						t.Fatalf("removed tool schema is loaded: %s", removed)
					}
				}
			}
		})
	}
}

func TestRegisterLimitsWebSearchFailuresAndUnproductiveResults(t *testing.T) {
	reg := newTestRegistry(t, testConfig())

	for _, name := range reg.EnabledBuiltinNames() {
		policy, ok := reg.Policy(name)
		if !ok {
			t.Fatalf("missing policy for %s", name)
		}
		wantExecutions := 0
		wantUnproductive := 0
		wantFailures := 0
		if name == websearch.Name {
			wantUnproductive = 2
			wantFailures = 2
		}
		if policy.MaxExecutions != wantExecutions {
			t.Fatalf("%s max executions = %d, want %d", name, policy.MaxExecutions, wantExecutions)
		}
		if policy.MaxUnproductive != wantUnproductive {
			t.Fatalf("%s max unproductive = %d, want %d", name, policy.MaxUnproductive, wantUnproductive)
		}
		if policy.MaxFailures != wantFailures {
			t.Fatalf("%s max failures = %d, want %d", name, policy.MaxFailures, wantFailures)
		}
	}
}

func TestRegisterImageGenerateProviderMatrix(t *testing.T) {
	for _, test := range []struct {
		name, url string
		enabled   bool
	}{
		{name: "blank"}, {name: "whitespace", url: "  "}, {name: "configured", url: "http://localhost:8188", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.ComfyUIURL = test.url
			cfg.ComfyUIGenerationTimeout = 2 * time.Minute
			reg := newTestRegistry(t, cfg)
			count := 0
			for _, tool := range reg.LLMTools() {
				if tool.Function.Name == imagegenerate.Name {
					count++
				}
			}
			if (count == 1) != test.enabled || count > 1 || reg.HasHandler(imagegenerate.Name) != test.enabled {
				t.Fatalf("image_generate advertised %d times, handler=%t", count, reg.HasHandler(imagegenerate.Name))
			}
			reserved := false
			for _, loaded := range reg.Names() {
				reserved = reserved || loaded == imagegenerate.Name
			}
			if !reserved {
				t.Fatal("image_generate is not reserved")
			}
			for _, legacy := range []string{"comfyui.text_to_image", "comfyui.image_to_image"} {
				if _, shown := visibleTestTool(reg, legacy); shown || reg.HasHandler(legacy) {
					t.Fatalf("legacy image tool available: %s", legacy)
				}
				for _, loaded := range reg.Names() {
					if loaded == legacy {
						t.Fatalf("legacy image tool reserved: %s", legacy)
					}
				}
			}
			if test.enabled {
				policy, ok := reg.Policy(imagegenerate.Name)
				if !ok || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult || !policy.BlockDuplicates || policy.NormalizeArgs == nil {
					t.Fatalf("image_generate policy=%+v", policy)
				}
			}
		})
	}
}

func TestRegisterImageGenerateSchema(t *testing.T) {
	cfg := testConfig()
	cfg.ComfyUIURL = "http://localhost:8188"
	cfg.ComfyUIGenerationTimeout = 2 * time.Minute
	reg := newTestRegistry(t, cfg)
	tool, ok := visibleTestTool(reg, imagegenerate.Name)
	if !ok {
		t.Fatal("missing image_generate")
	}
	schema := tool.Function.Parameters
	if len(schema.Properties) != 3 || len(schema.Required) != 1 || schema.Required[0] != "prompt" || schema.AdditionalProperties != nil {
		t.Fatalf("image_generate schema=%+v", schema)
	}
	prompt := schema.Properties["prompt"]
	if prompt.Type != "string" || prompt.MinLength != nil || prompt.MaxLength != nil {
		t.Fatalf("prompt schema=%+v", prompt)
	}
	if ratio := schema.Properties["aspect_ratio"]; ratio.Type != "string" || ratio.Default != "landscape" || !reflect.DeepEqual(ratio.Enum, []string{"landscape", "square", "portrait"}) {
		t.Fatalf("aspect_ratio schema=%+v", ratio)
	}
	if source := schema.Properties["image_url"]; source.Type != "string" {
		t.Fatalf("image_url schema=%+v", source)
	}
}

func TestImageGenerateFingerprintScope(t *testing.T) {
	cfg := testConfig()
	cfg.ComfyUIURL = "http://localhost:8188"
	cfg.ComfyUIGenerationTimeout = 2 * time.Minute
	reg := newTestRegistry(t, cfg)
	name := imagegenerate.Name
	policy, ok := reg.Policy(name)
	if !ok || !policy.BlockDuplicates || policy.NormalizeArgs == nil {
		t.Fatalf("missing image_generate duplicate policy: %+v", policy)
	}
	fingerprint := func(args map[string]interface{}) string {
		t.Helper()
		got, err := governance.Fingerprint(name, args, policy.NormalizeArgs)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := fingerprint(map[string]interface{}{"prompt": "a blue car"})
	if fingerprint(map[string]interface{}{"prompt": " a  blue car ", "aspect_ratio": "landscape"}) != base {
		t.Fatal("default aspect ratio or prompt whitespace changed fingerprint")
	}
	if fingerprint(map[string]interface{}{"prompt": "a blue car", "aspect_ratio": "square"}) == base {
		t.Fatal("different aspect ratio shares fingerprint")
	}
	if fingerprint(map[string]interface{}{"prompt": "a blue car", "image_url": "current-1"}) == base {
		t.Fatal("image source shares text-to-image fingerprint")
	}
	if fingerprint(map[string]interface{}{"prompt": "a blue car", "image_url": "current-2"}) == fingerprint(map[string]interface{}{"prompt": "a blue car", "image_url": "current-1"}) {
		t.Fatal("different image sources share fingerprint")
	}
}
