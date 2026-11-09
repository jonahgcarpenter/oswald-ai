package startup

import (
	"sort"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

// publishPluginEnvPassthrough registers every installed plugin's declared
// requires_env names for sandbox passthrough so the plugin's CLIs receive
// them in terminal, execute_code, and background children from the profile
// dotenv scope. Refused credential names are skipped and reported by count
// only; values are never logged.
func publishPluginEnvPassthrough(manager *plugins.Manager, profile string, log *config.Logger) {
	if manager == nil {
		return
	}
	declared := manager.EnabledRequiresEnv()
	keys := make([]string, 0, len(declared))
	for key := range declared {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if refused := process.RegisterEnvPassthrough(declared[key]); len(refused) > 0 {
			log.Warn("plugin.manager.passthrough_refused", "plugin requested refused environment variables", config.F("plugin", key), config.F("profile", profile), config.F("refused_count", len(refused)))
		}
	}
}
