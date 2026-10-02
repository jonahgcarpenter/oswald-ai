package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestProfileMCPIsolationAndImmutableConfiguration(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	entries := []config.MCPServer{{Name: "example", URL: "https://example.com/mcp", Description: "Synthetic tools.", Transport: TransportStreamableHTTP, Enabled: true, Headers: map[string]string{"Authorization": "Bearer alice"}}}
	alice, err := NewProfileManager("alice", entries, log)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	entries[0].Headers["Authorization"] = "Bearer changed"
	bob, err := NewProfileManager("bob", entries, log)
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
	tools := NewProvider(alice).DiscoveryTools(ctx, testPrincipal("alice"))
	if len(tools) != 1 || tools[0].Function.Name != "example.tools" {
		t.Fatal("profile discovery was not advertised")
	}
	if len(NewProvider(alice).DiscoveryTools(ctx, testPrincipal("bob"))) != 0 {
		t.Fatal("another profile received discovery tools")
	}
}

func TestProfileMCPCloseFencesLateConnectionAndIsIdempotent(t *testing.T) {
	manager, err := NewProfileManager("alice", nil, config.NewLogger(config.LevelError))
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
