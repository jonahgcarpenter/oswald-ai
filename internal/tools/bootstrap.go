package tools

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/websearch"
)

// NewRegistryFromConfig creates a Registry, loads tool definitions, and wires builtin tools.
func NewRegistryFromConfig(cfg *config.Config, log *config.Logger) (*Registry, error) {
	registry, err := NewRegistryFromDirectory(cfg.ToolsConfig, log)
	if err != nil {
		return nil, err
	}

	if err := registerBuiltins(registry, cfg, log); err != nil {
		return nil, err
	}

	log.Info("Tool registry: %d tool(s) loaded from %s", registry.Count(), cfg.ToolsConfig)
	return registry, nil
}

// registerBuiltins wires all builtin tools into the shared registry.
func registerBuiltins(registry *Registry, cfg *config.Config, log *config.Logger) error {
	searchClient := websearch.NewClient(cfg.SearxngURL, log)
	if err := registry.RegisterHandler("web_search", Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web_search tool: %w", err)
	}

	log.Info("Tools: web search client configured: %s", cfg.SearxngURL)

	return nil
}
