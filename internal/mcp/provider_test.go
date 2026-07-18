package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestProviderDiscoveryToolsAreScopedToVisibleEnabledServers(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "oswald.db"), "12345678901234567890123456789012", config.NewLogger(config.LevelError).Server("test"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	store.SetResolverForTest(staticResolver{"example.com": {"93.184.216.34"}})
	ctx := context.Background()
	for _, cfg := range []ServerConfig{
		{Scope: ScopeGlobal, Name: "github", Transport: TransportStreamableHTTP, URL: "https://example.com/github", Enabled: true},
		{Scope: ScopeUser, OwnerUserID: "user_1", Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/home", Enabled: true},
		{Scope: ScopeUser, OwnerUserID: "user_2", Name: "other", Transport: TransportStreamableHTTP, URL: "https://example.com/other", Enabled: true},
		{Scope: ScopeUser, OwnerUserID: "user_1", Name: "disabled", Transport: TransportStreamableHTTP, URL: "https://example.com/disabled", Enabled: false},
	} {
		if _, err := store.Save(ctx, cfg); err != nil {
			t.Fatalf("save %s: %v", cfg.Name, err)
		}
	}
	provider := NewProvider(NewManagerFromStore(store, config.NewLogger(config.LevelError)))
	tools := provider.DiscoveryTools(ctx, testPrincipal("user_1"))
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Function.Name] = true
	}
	if !names["github.tools"] || !names["home.tools"] {
		t.Fatalf("missing visible discovery tools: %+v", names)
	}
	if names["other.tools"] || names["disabled.tools"] {
		t.Fatalf("unexpected discovery tools: %+v", names)
	}
	for _, tool := range tools {
		if tool.Function.Name != "home.tools" {
			continue
		}
		if _, ok := tool.Function.Parameters.Properties["query"]; !ok {
			t.Fatal("home.tools schema missing query parameter")
		}
		if _, ok := tool.Function.Parameters.Properties["limit"]; ok {
			t.Fatal("home.tools schema unexpectedly includes limit parameter")
		}
	}
	invalid := identity.Principal{CanonicalUserID: "user_1", Gateway: "websocket", ExternalID: "user_1", Assurance: identity.AssuranceDiscordGateway}
	if tools := provider.DiscoveryTools(ctx, invalid); len(tools) != 0 {
		t.Fatalf("invalid principal received discovery tools: %+v", tools)
	}
}

func TestProviderResolveToolsUsesCurrentVisibleCatalog(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "oswald.db"), "12345678901234567890123456789012", config.NewLogger(config.LevelError).Server("test"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	store.SetResolverForTest(staticResolver{"example.com": {"93.184.216.34"}})
	ctx := context.Background()
	cfg, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "user_1", Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/home", Enabled: true})
	if err != nil {
		t.Fatalf("save home: %v", err)
	}
	manager := NewManagerFromStore(store, config.NewLogger(config.LevelError))
	manager.sessions[scopeKey(cfg)] = &server{config: cfg, tools: []ToolSpec{
		{Name: "home.turn_on", Server: "home", RemoteName: "turn_on"},
		{Name: "home.weather", Server: "home", RemoteName: "weather"},
	}}
	provider := NewProvider(manager)

	resolved := provider.ResolveTools(ctx, testPrincipal("user_1"), []string{"home.turn_on", "home.tools", "web.search", "home.missing", "home.turn_on"})
	if len(resolved) != 1 || resolved[0] != "home.turn_on" {
		t.Fatalf("resolved tools = %+v", resolved)
	}
	if resolved := provider.ResolveTools(ctx, testPrincipal("user_2"), []string{"home.turn_on"}); len(resolved) != 0 {
		t.Fatalf("resolved another user's tools: %+v", resolved)
	}
}

func testPrincipal(userID string) identity.Principal {
	return identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: userID, Assurance: identity.AssuranceSelfAsserted}
}

func TestSearchToolsReturnsAllToolsWithoutQuery(t *testing.T) {
	catalog := []registryEntry{
		{name: "home.turn_on", description: "Turn on a light"},
		{name: "home.turn_off", description: "Turn off a light"},
		{name: "home.weather", description: "Read weather"},
	}
	entries := makeCatalog(catalog)
	tools := searchTools(entries, "home", "")
	if len(tools) != 3 {
		t.Fatalf("tool count = %d, want 3: %+v", len(tools), tools)
	}
	if tools[0].Name != "home.turn_off" || tools[1].Name != "home.turn_on" || tools[2].Name != "home.weather" {
		t.Fatalf("unexpected search result: %+v", tools)
	}
}

func TestSearchToolsFiltersByQueryWithoutLimit(t *testing.T) {
	catalog := []registryEntry{
		{name: "home.turn_on", description: "Turn on a light"},
		{name: "home.turn_off", description: "Turn off a light"},
		{name: "home.weather", description: "Read weather"},
	}
	entries := makeCatalog(catalog)
	tools := searchTools(entries, "home", "light")
	if len(tools) != 2 {
		t.Fatalf("tool count = %d, want 2: %+v", len(tools), tools)
	}
	if tools[0].Name != "home.turn_off" || tools[1].Name != "home.turn_on" {
		t.Fatalf("unexpected search result: %+v", tools)
	}
}

type registryEntry struct {
	name        string
	description string
}

func makeCatalog(entries []registryEntry) []registry.CatalogEntry {
	out := make([]registry.CatalogEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, registry.CatalogEntry{Name: entry.name, Description: entry.description, Source: registry.ToolSourceMCP, Server: "home"})
	}
	return out
}
