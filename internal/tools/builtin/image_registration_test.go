package builtin

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/names"
)

func TestImageToolsAreBraveOnlyWithBoundedMetadataPolicies(t *testing.T) {
	for _, key := range []string{"", "synthetic"} {
		log := config.NewLogger(config.LevelError)
		reg := newTestRegistry(t, log)
		cfg := testConfig()
		cfg.BraveAPIKey = key
		if err := Register(reg, cfg, nil, nil, log); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{names.WebImageSearch, names.WebImageSelect} {
			_, visible := visibleTestTool(reg, name)
			if visible != (key != "") || reg.HasHandler(name) != (key != "") {
				t.Fatal("Brave enablement", name)
			}
			if key == "" {
				continue
			}
			policy, ok := reg.Policy(name)
			if !ok || policy.History.Mode != governance.HistoryMetadata || policy.History.SearchResult {
				t.Fatal("durable history policy")
			}
			if name == names.WebImageSearch && (policy.MaxExecutions != 2 || policy.MaxFailures != 2 || policy.MaxUnproductive != 2) {
				t.Fatal("search bounds")
			}
		}
	}
}

func TestImageSearchOptionalResultLimitSchema(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg := newTestRegistry(t, log)
	cfg := testConfig()
	cfg.BraveAPIKey = "synthetic"
	if err := Register(reg, cfg, nil, nil, log); err != nil {
		t.Fatal(err)
	}
	tool, ok := visibleTestTool(reg, names.WebImageSearch)
	if !ok {
		t.Fatal("image search schema is unavailable")
	}
	schema := tool.Function.Parameters
	results := schema.Properties["results"]
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties || len(schema.Properties) != 2 || len(schema.Required) != 1 || schema.Required[0] != "query" || schema.Properties["query"].Type != "string" {
		t.Fatalf("image search schema is not strict: %+v", schema)
	}
	if results.Type != "integer" || results.Minimum == nil || *results.Minimum != 1 || results.Maximum == nil || *results.Maximum != websearch.MaxImageResults || !strings.Contains(results.Description, "defaults to 2") {
		t.Fatalf("image search results schema = %+v", results)
	}
}
