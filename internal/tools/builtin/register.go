package builtin

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/currenttime"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/websearch"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Register wires all builtin tools into the shared registry.
func Register(reg *registry.Registry, cfg *config.Config, userMemStore *usermemory.Store, globalMemStore *globalmemory.Store, log *config.Logger) error {
	bootstrapLog := log.Server("tool.bootstrap")
	searchClient, err := websearch.NewClient(cfg.SearxngURL, log.Server("tool.web.search"))
	if err != nil {
		return fmt.Errorf("failed to initialize web.search client: %w", err)
	}
	searchPolicy := toolPolicy(2, normalizeSearchArgs)
	searchPolicy.MaxFailures = 2
	if err := reg.RegisterHandler("web.search", searchPolicy, registry.Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web.search tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured web search tool", config.F("tool_name", "web.search"))

	if err := reg.RegisterHandler("time.current", toolPolicy(0, normalizeTimeArgs), registry.Handler(currenttime.NewHandler(time.Now))); err != nil {
		return fmt.Errorf("failed to initialize time.current tool: %w", err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured current time tool", config.F("tool_name", "time.current"))

	if err := reg.RegisterHandler(toolnames.UserMemorySearch, toolPolicy(0, normalizeMemorySearchArgs(8)), registry.Handler(usermemory.NewSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemorySearch, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemorySearch), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler(toolnames.UserMemoryList, toolPolicy(0, normalizeMemorySearchArgs(25)), registry.Handler(usermemory.NewListHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.UserMemoryList, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured user memory tool", config.F("tool_name", toolnames.UserMemoryList), config.F("path", config.DefaultAccountLinkPath))

	if err := reg.RegisterHandler(toolnames.SessionTranscriptSearch, toolPolicy(0, normalizeMemorySearchArgs(5)), registry.Handler(usermemory.NewTranscriptSearchHandler(userMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.SessionTranscriptSearch, err)
	}

	if err := reg.RegisterHandler(toolnames.GlobalMemorySearch, toolPolicy(0, normalizeMemorySearchArgs(globalmemory.DefaultSearchLimit)), registry.Handler(globalmemory.NewSearchHandler(globalMemStore, log))); err != nil {
		return fmt.Errorf("failed to initialize %s tool: %w", toolnames.GlobalMemorySearch, err)
	}
	bootstrapLog.Debug("tool.bootstrap.configured", "configured session transcript tool", config.F("tool_name", toolnames.SessionTranscriptSearch))
	bootstrapLog.Debug("tool.bootstrap.configured", "configured global memory tool", config.F("tool_name", toolnames.GlobalMemorySearch))

	return nil
}

func toolPolicy(maxUnproductive int, normalize governance.ArgumentNormalizer) governance.ToolPolicy {
	return governance.ToolPolicy{
		MaxUnproductive: maxUnproductive,
		BlockDuplicates: true,
		NormalizeArgs:   normalize,
	}
}

func normalizeSearchArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"query": normalizedString(args, "query", true)}
}

func normalizeTimeArgs(args map[string]interface{}) interface{} {
	return map[string]interface{}{"timezone": normalizedString(args, "timezone", false)}
}

func normalizeMemorySearchArgs(defaultLimit int) governance.ArgumentNormalizer {
	return func(args map[string]interface{}) interface{} {
		var limit interface{} = defaultLimit
		if raw, exists := args["limit"]; exists && raw != nil {
			if value, ok := numericInt(raw); ok {
				limit = value
			} else {
				limit = map[string]interface{}{"invalid": raw}
			}
		}
		return map[string]interface{}{
			"query":    normalizedString(args, "query", true),
			"scope":    normalizedString(args, "scope", true),
			"category": normalizedString(args, "category", true),
			"limit":    limit,
		}
	}
}

func normalizedString(args map[string]interface{}, key string, lower bool) string {
	value, _ := args[key].(string)
	value = strings.Join(strings.Fields(value), " ")
	if lower {
		value = strings.ToLower(value)
	}
	return value
}

func numericInt(value interface{}) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), true
	case float64:
		if !math.IsNaN(number) && !math.IsInf(number, 0) && number == math.Trunc(number) && float64(int(number)) == number {
			return int(number), true
		}
	case float32:
		value := float64(number)
		if !math.IsNaN(value) && !math.IsInf(value, 0) && number == float32(int(number)) {
			return int(number), true
		}
	}
	return 0, false
}
