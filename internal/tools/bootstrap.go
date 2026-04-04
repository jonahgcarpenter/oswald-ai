package tools

import (
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/soulmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/usermemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/websearch"
)

// NewRegistryFromConfig creates a Registry, loads tool definitions, and wires builtin tools.
// The soul store and user memory store are created externally and passed in so the agent
// can share the same instances with the tool handlers.
// chatClient and model are forwarded to the persistent_memory handler so it can perform
// LLM-based migration of old flat-format user memory files on first recall.
func NewRegistryFromConfig(cfg *config.Config, soulStore *soulmemory.Store, userMemStore *usermemory.Store, chatClient ollama.Chatter, model string, log *config.Logger) (*Registry, error) {
	registry, err := NewRegistryFromDirectory(config.DefaultToolsConfigDir, log)
	if err != nil {
		return nil, err
	}

	if err := registerBuiltins(registry, cfg, soulStore, userMemStore, chatClient, model, log); err != nil {
		return nil, err
	}

	log.Info("Tools enabled: %s", strings.Join(registry.Names(), ", "))
	return registry, nil
}

// registerBuiltins wires all builtin tools into the shared registry.
func registerBuiltins(registry *Registry, cfg *config.Config, soulStore *soulmemory.Store, userMemStore *usermemory.Store, chatClient ollama.Chatter, model string, log *config.Logger) error {
	searchClient := websearch.NewClient(cfg.SearxngURL, log)
	if err := registry.RegisterHandler("web_search", Handler(websearch.NewHandler(searchClient, log))); err != nil {
		return fmt.Errorf("failed to initialize web_search tool: %w", err)
	}
	log.Debug("Tools: web search client configured: %s", cfg.SearxngURL)

	if err := registry.RegisterHandler("persistent_memory", Handler(usermemory.NewHandler(userMemStore, chatClient, model, log))); err != nil {
		return fmt.Errorf("failed to initialize persistent_memory tool: %w", err)
	}
	log.Debug("Tools: persistent user memory configured: %s", config.DefaultUserMemoryPath)

	if err := registry.RegisterHandler("soul_memory", Handler(soulmemory.NewHandler(soulStore, log))); err != nil {
		return fmt.Errorf("failed to initialize soul_memory tool: %w", err)
	}
	log.Debug("Tools: soul memory configured: %s", config.DefaultSoulPath)

	return nil
}
