package builtin

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
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
