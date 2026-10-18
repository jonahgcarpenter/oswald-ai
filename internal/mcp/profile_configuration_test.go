package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func TestProfileMCPIsolationAndImmutableConfiguration(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	entries := []config.MCPServer{{Name: "example", URL: "https://example.com/mcp", Transport: TransportStreamableHTTP, Enabled: true, Headers: map[string]string{"Authorization": "Bearer alice"}}}
	alice, err := NewProfileManager("alice", t.TempDir(), entries, log)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	entries[0].Headers["Authorization"] = "Bearer changed"
	bob, err := NewProfileManager("bob", t.TempDir(), entries, log)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	ctx := context.Background()
	if len(alice.ServerInfos(ctx, "bob")) != 0 || len(bob.ServerInfos(ctx, "alice")) != 0 {
		t.Fatal("MCP servers crossed profile ownership")
	}
	cfg, ok, err := alice.resolveConfig(ctx, "alice", "example")
	if err != nil || !ok {
		t.Fatal("missing profile MCP configuration", err)
	}
	if cfg.Headers["Authorization"] != "Bearer alice" {
		t.Fatal("caller mutated resolved profile credentials")
	}
	tools := NewProvider(alice).BridgeTools(ctx, testPrincipal("alice"))
	if len(tools) != 3 {
		t.Fatal("profile bridge was not advertised")
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Function.Name] = true
	}
	if !names[ToolSearch] || !names[ToolDescribe] || !names[ToolCall] {
		t.Fatalf("profile bridge tools = %+v", names)
	}
	if got := NewProvider(alice).BridgeTools(ctx, testPrincipal("bob")); len(got) != 3 {
		t.Fatal("bridge must be advertised per principal without leaking catalog")
	}
	// Catalog isolation is enforced at search time, not advertisement time:
	// bob's search must not return alice's deferred tools.
	search, _, err := NewProvider(alice).Execute(ctx, testPrincipal("bob"), ToolSearch, map[string]interface{}{"queries": []interface{}{"synthetic"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(search.Content, "mcp__example__") {
		t.Fatal("MCP catalog crossed profile ownership")
	}
}

func TestProfileMCPCloseFencesLateConnectionAndIsIdempotent(t *testing.T) {
	manager, err := NewProfileManager("alice", t.TempDir(), nil, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	manager.sessions["synthetic"] = &server{close: func() error { closed++; return nil }}
	generation := manager.generation
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager.rememberError("late", ServerConfig{}, generation, errors.New("synthetic failure"))
	if closed != 1 || len(manager.sessions) != 0 {
		t.Fatal("close duplicated work or allowed late cache publication")
	}
	if _, err := manager.ensureConnected(context.Background(), ServerConfig{}); err == nil {
		t.Fatal("closed manager accepted a connection")
	}
}
