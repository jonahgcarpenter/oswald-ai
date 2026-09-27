package builtin

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	toolnames "github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func testConfig() *config.Config {
	return &config.Config{SearxngURL: "http://localhost:8080"}
}

func visibleTestTool(reg *registry.Registry, name string) (llm.Tool, bool) {
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name == name {
			return tool, true
		}
	}
	return llm.Tool{}, false
}

func newTestRegistry(t *testing.T, log *config.Logger) *registry.Registry {
	t.Helper()
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRegisterDoesNotExposeSoulTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
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

func TestRegisterIncludesFileMemoryTool(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	if !reg.HasHandler(toolnames.Memory) {
		t.Fatal("memory handler was not registered")
	}
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name != toolnames.Memory {
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
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	if _, ok := visibleTestTool(reg, toolnames.SessionTranscriptSearch); ok || reg.HasHandler(toolnames.SessionTranscriptSearch) {
		t.Fatal("transcript search is available")
	}
}

func TestRegisterCatalogOmitsRemovedMemoryTools(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
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
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"web_search":     true,
		toolnames.Memory: true,
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
	log := config.NewLogger(config.LevelError)
	reg := newTestRegistry(t, log)
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	policy, ok := reg.Policy(toolnames.Memory)
	if !ok || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult {
		t.Fatalf("unexpected file memory policy: %+v", policy)
	}
	tool, ok := visibleTestTool(reg, toolnames.Memory)
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
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	wantParameter := map[string]string{"web_search": "query"}
	for _, tool := range reg.LLMTools() {
		parameter, exists := wantParameter[tool.Function.Name]
		if !exists {
			continue
		}
		schema := tool.Function.Parameters
		limit := schema.Properties["limit"]
		if limit.Type != "integer" || limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != websearch.MaxWebResults || limit.Default == nil || *limit.Default != websearch.DefaultWebResults || limit.Description != "Maximum number of results to return. Defaults to 5." {
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
	log := config.NewLogger(config.LevelError)
	reg := newTestRegistry(t, log)
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}
	const parameter = "limit"
	name := toolnames.WebSearch
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
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{BraveAPIKey: "secret", SearxngURL: "localhost:8080"}, nil, nil, log); err == nil || !strings.Contains(err.Error(), "SearXNG web_search client") {
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
			for _, toolName := range []string{toolnames.WebSearch} {
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
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", config.DefaultDataRoot, "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, testConfig(), nil, nil, log); err != nil {
		t.Fatal(err)
	}

	for _, name := range reg.EnabledBuiltinNames() {
		policy, ok := reg.Policy(name)
		if !ok {
			t.Fatalf("missing policy for %s", name)
		}
		wantExecutions := 0
		wantUnproductive := 0
		wantFailures := 0
		if name == "web_search" {
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

func TestRegisterComfyUIProviderMatrix(t *testing.T) {
	for _, test := range []struct {
		name, url string
		enabled   bool
	}{
		{name: "blank"}, {name: "whitespace", url: "  "}, {name: "configured", url: "http://localhost:8188", enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := config.NewLogger(config.LevelError)
			reg := newTestRegistry(t, log)
			cfg := testConfig()
			cfg.ComfyUIURL = test.url
			cfg.ComfyUIGenerationTimeout = 2 * time.Minute
			cfg.ComfyUITextToImageWorkflowPath = filepath.Join("..", "..", "..", config.DefaultDataRoot, "workflows", "comfyui", "text-to-image-basic.json")
			cfg.ComfyUIImageToImageWorkflowPath = filepath.Join("..", "..", "..", config.DefaultDataRoot, "workflows", "comfyui", "image-to-image-basic.json")
			if err := Register(reg, cfg, nil, nil, log); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
				_, visible := visibleTestTool(reg, name)
				if visible != test.enabled || reg.HasHandler(name) != test.enabled {
					t.Fatalf("%s visible=%t handler=%t", name, visible, reg.HasHandler(name))
				}
				reserved := false
				for _, loaded := range reg.Names() {
					reserved = reserved || loaded == name
				}
				if !reserved {
					t.Fatalf("%s is not reserved", name)
				}
				if test.enabled {
					policy, ok := reg.Policy(name)
					if !ok || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult {
						t.Fatalf("%s policy=%+v", name, policy)
					}
				}
			}
		})
	}
}

func TestRegisterComfyUISchemasExposePromptsAndImageSource(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg := newTestRegistry(t, log)
	cfg := testConfig()
	cfg.ComfyUIURL = "http://localhost:8188"
	cfg.ComfyUIGenerationTimeout = 2 * time.Minute
	cfg.ComfyUITextToImageWorkflowPath = filepath.Join("..", "..", "..", config.DefaultDataRoot, "workflows", "comfyui", "text-to-image-basic.json")
	cfg.ComfyUIImageToImageWorkflowPath = filepath.Join("..", "..", "..", config.DefaultDataRoot, "workflows", "comfyui", "image-to-image-basic.json")
	if err := Register(reg, cfg, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{toolnames.ComfyUITextToImage, toolnames.ComfyUIImageToImage} {
		tool, ok := visibleTestTool(reg, name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		schema := tool.Function.Parameters
		wantProperties := 2
		if name == toolnames.ComfyUIImageToImage {
			wantProperties = 5
			if schema.Properties["create_variant"].Type != "boolean" {
				t.Fatal("missing boolean variant selector")
			}
			strength, ok := schema.Properties["strength"]
			if !ok || strength.Type != "number" || strength.Minimum == nil || *strength.Minimum != 0.1 || strength.Maximum == nil || *strength.Maximum != 0.9 {
				t.Fatalf("missing strength bounds: %+v", strength)
			}
			if _, ok := schema.Properties["source_image_id"]; !ok {
				t.Fatal("missing image source selector")
			}
		}
		if len(schema.Properties) != wantProperties || len(schema.Required) != 1 || schema.Required[0] != "prompt" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Fatalf("%s schema=%+v", name, schema)
		}
		prompt, ok := schema.Properties["prompt"]
		if !ok {
			t.Fatalf("%s missing prompt", name)
		}
		negative, ok := schema.Properties["negative_prompt"]
		if !ok {
			t.Fatalf("%s missing negative_prompt", name)
		}
		if prompt.MinLength == nil || *prompt.MinLength != 1 || prompt.MaxLength == nil || *prompt.MaxLength != 2000 || negative.MaxLength == nil || *negative.MaxLength != 2000 {
			t.Fatalf("%s prompt bounds are missing: %+v", name, schema.Properties)
		}
	}
}

func TestComfyStrengthFingerprintScope(t *testing.T) {
	for _, test := range []struct {
		name      string
		normalize governance.ArgumentNormalizer
		wantSame  bool
	}{
		{toolnames.ComfyUITextToImage, normalizeComfyArgs, true},
		{toolnames.ComfyUIImageToImage, normalizeComfyImageArgs, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := map[string]interface{}{"prompt": "a blue car", "source_image_id": "current-1"}
			omitted, err := governance.Fingerprint(test.name, args, test.normalize)
			if err != nil {
				t.Fatal(err)
			}
			args["strength"] = 0.6
			explicit, err := governance.Fingerprint(test.name, args, test.normalize)
			if err != nil {
				t.Fatal(err)
			}
			if (omitted == explicit) != test.wantSame {
				t.Fatal("unexpected strength fingerprint scope")
			}
		})
	}
}
