package tools

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/soulmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/websearch"
)

// NewRegistryFromConfig creates a Registry, loads tool definitions, and wires builtin tools.
// The soul store is created externally and passed in so the agent can also hold a reference
// to it for reading the system prompt on every request.
func NewRegistryFromConfig(cfg *config.Config, soulStore *soulmemory.Store, log *config.Logger) (*Registry, error) {
	registry, err := NewRegistryFromDirectory(cfg.ToolsConfig, log)
	if err != nil {
		return nil, err
	}

	if err := registerBuiltins(registry, cfg, soulStore, log); err != nil {
		return nil, err
	}

	log.Info("Tools enabled: %s", strings.Join(registry.Names(), ", "))
	return registry, nil
}

// registerBuiltins wires all builtin tools into the shared registry.
func registerBuiltins(registry *Registry, cfg *config.Config, soulStore *soulmemory.Store, log *config.Logger) error {
	searchClient := websearch.NewClient(cfg.SearxngURL, log)
	if err := registry.RegisterHandler("web_search", Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web_search tool: %w", err)
	}
	log.Debug("Tools: web search client configured: %s", cfg.SearxngURL)

	memStore := usermemory.NewStore(cfg.UserMemoryPath, log)
	if err := registry.RegisterHandler("persistent_memory", Handler(usermemory.NewHandler(memStore, log))); err != nil {
		return fmt.Errorf("failed to initialize persistent_memory tool: %w", err)
	}
	log.Debug("Tools: persistent user memory configured: %s", cfg.UserMemoryPath)

	if err := registry.RegisterHandler("soul_memory", Handler(soulmemory.NewHandler(soulStore, log))); err != nil {
		return fmt.Errorf("failed to initialize soul_memory tool: %w", err)
	}
	log.Debug("Tools: soul memory configured: %s", cfg.SoulPath)

	return nil
}
