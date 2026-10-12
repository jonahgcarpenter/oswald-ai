package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// syntheticMCPServer builds a test MCP server with an echo tool and a second
// tool used to exercise include/exclude filtering.
func syntheticMCPServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	type echoArgs struct {
		Text string `json:"text"`
	}
	gomcp.AddTool(server, &gomcp.Tool{Name: "echo", Description: "Echo input text"},
		func(_ context.Context, _ *gomcp.CallToolRequest, args echoArgs) (*gomcp.CallToolResult, any, error) {
			return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "echo:" + args.Text}}}, nil, nil
		})
	gomcp.AddTool(server, &gomcp.Tool{Name: "hidden_tool", Description: "Filtered from catalogs"},
		func(context.Context, *gomcp.CallToolRequest, any) (*gomcp.CallToolResult, any, error) {
			return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "hidden"}}}, nil, nil
		})
	return server
}

// TestStdioHelperChild runs a synthetic MCP server over stdio when spawned as
// a child process by TestConnectStdioRoundTrip.
func TestStdioHelperChild(t *testing.T) {
	if os.Getenv("OSWALD_TEST_STDIO_HELPER") == "" {
		t.Skip("stdio helper child only")
	}
	if err := syntheticMCPServer().Run(context.Background(), &gomcp.StdioTransport{}); err != nil {
		t.Fatalf("stdio helper: %v", err)
	}
}

func stdioHelperCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	// No -test.v: verbose harness output would pollute the child's stdout and
	// break the stdio JSON-RPC stream.
	cmd := exec.Command(os.Args[0], "-test.run=TestStdioHelperChild")
	cmd.Env = append(os.Environ(), "OSWALD_TEST_STDIO_HELPER=1")
	return cmd
}

func TestConnectStdioRoundTrip(t *testing.T) {
	helper := stdioHelperCommand(t)
	cfg := ServerConfig{Name: "local", Transport: TransportStdio, Command: helper.Path, Args: helper.Args[1:], Env: map[string]string{"OSWALD_TEST_STDIO_HELPER": "1"}, Enabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, closeFn, err := connectStdio(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	specs, err := loadToolSpecs(ctx, cfg, session, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "mcp__local__echo" || specs[1].Name != "mcp__local__hidden_tool" {
		names := make([]string, 0, len(specs))
		for _, spec := range specs {
			names = append(names, spec.Name)
		}
		t.Fatalf("stdio catalog = %+v", names)
	}
	result, err := specs[0].Handler(ctx, map[string]interface{}{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "echo:hi") {
		t.Fatalf("stdio result = %q", result.Content)
	}
}

func TestConnectStdioRejectsMissingCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := connectStdio(ctx, ServerConfig{Name: "local", Command: "oswald-definitely-missing-binary"}); err == nil {
		t.Fatal("missing stdio command accepted")
	}
	if _, _, err := connectStdio(ctx, ServerConfig{Name: "local"}); err == nil {
		t.Fatal("empty stdio command accepted")
	}
}

func TestConnectSSERoundTrip(t *testing.T) {
	handler := gomcp.NewSSEHandler(func(*http.Request) *gomcp.Server { return syntheticMCPServer() }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	cfg := ServerConfig{Name: "sse", Transport: TransportSSE, URL: server.URL, Enabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, closeFn, err := connectSSE(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	specs, err := loadToolSpecs(ctx, cfg, session, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "mcp__sse__echo" {
		t.Fatalf("SSE catalog = %+v", specs)
	}
}

func TestLoadToolSpecsHonorsIncludeExclude(t *testing.T) {
	handler := gomcp.NewSSEHandler(func(*http.Request) *gomcp.Server { return syntheticMCPServer() }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log := config.NewLogger(config.LevelError)
	load := func(include, exclude []string) []string {
		t.Helper()
		cfg := ServerConfig{Name: "filtered", Transport: TransportSSE, URL: server.URL, Include: include, Exclude: exclude, Enabled: true}
		session, closeFn, err := connectSSE(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer closeFn()
		specs, err := loadToolSpecs(ctx, cfg, session, log)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(specs))
		for _, spec := range specs {
			names = append(names, spec.Name)
		}
		return names
	}
	if names := load(nil, nil); len(names) != 2 {
		t.Fatalf("unfiltered catalog = %+v", names)
	}
	if names := load([]string{"echo"}, nil); len(names) != 1 || names[0] != "mcp__filtered__echo" {
		t.Fatalf("include catalog = %+v", names)
	}
	if names := load([]string{"echo", "hidden_tool"}, []string{"echo"}); len(names) != 1 || names[0] != "mcp__filtered__hidden_tool" {
		t.Fatalf("exclude-wins catalog = %+v", names)
	}
	if names := load([]string{"missing"}, nil); len(names) != 0 {
		t.Fatalf("unknown include catalog = %+v", names)
	}
}

func TestBridgeSearchRespectsIncludeExcludeFilter(t *testing.T) {
	handler := gomcp.NewSSEHandler(func(*http.Request) *gomcp.Server { return syntheticMCPServer() }, nil)
	server := httptest.NewServer(handler)
	defer server.Close()
	store := testStore(t)
	addTestUsers(t, store, "user_1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "user_1", Name: "filtered", Transport: TransportSSE, URL: server.URL, Exclude: []string{"hidden_tool"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	manager := newManager(store, config.NewLogger(config.LevelError))
	defer manager.Close()
	provider := NewProvider(manager)
	result, _, err := provider.Execute(ctx, testPrincipal("user_1"), ToolSearch, map[string]interface{}{"queries": []interface{}{"tool", "echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, "hidden_tool") {
		t.Fatalf("excluded tool leaked into search: %s", result.Content)
	}
	if !strings.Contains(result.Content, "mcp__filtered__echo") {
		t.Fatalf("included tool missing from search: %s", result.Content)
	}
	_ = cfg
}

func TestToolAllowedSemantics(t *testing.T) {
	if !toolAllowed(nil, nil, "echo") || toolAllowed(nil, []string{"echo"}, "echo") {
		t.Fatal("empty include / exclude mismatch")
	}
	if !toolAllowed([]string{"echo"}, nil, "echo") || toolAllowed([]string{"other"}, nil, "echo") {
		t.Fatal("include mismatch")
	}
	if toolAllowed([]string{"echo"}, []string{"echo"}, "echo") {
		t.Fatal("exclude must win over include")
	}
}

func TestProfileManagerAcceptsStdioAndSSE(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	manager, err := NewProfileManager("alice", []config.MCPServer{
		{Name: "local", Command: "npx", Args: []string{"-y", "x"}, Cwd: ".", Env: map[string]string{"A": "b"}, Transport: TransportStdio, Exclude: []string{"edit"}, Enabled: true},
		{Name: "remote", URL: "http://localhost:8080/mcp", Transport: TransportSSE, Enabled: true},
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	configs, err := manager.store.ListForUser(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 {
		t.Fatalf("servers = %+v", configs)
	}
	for _, cfg := range configs {
		switch cfg.Name {
		case "local":
			if cfg.Transport != TransportStdio || cfg.Command != "npx" || len(cfg.Args) != 2 || cfg.Cwd != "." || cfg.Env["A"] != "b" || len(cfg.Exclude) != 1 {
				t.Fatalf("stdio server = %+v", cfg)
			}
		case "remote":
			if cfg.Transport != TransportSSE || cfg.URL != "http://localhost:8080/mcp" {
				t.Fatalf("sse server = %+v", cfg)
			}
		default:
			t.Fatalf("unexpected server %q", cfg.Name)
		}
	}
}

func TestProfileManagerRejectsMixedTransports(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	for _, servers := range [][]config.MCPServer{
		{{Name: "bad", URL: "https://example.com/mcp", Command: "npx", Enabled: true}},
		{{Name: "bad", Command: "npx", Transport: "sse", Enabled: true}},
		{{Name: "bad", URL: "https://example.com/mcp", Args: []string{"x"}, Enabled: true}},
		{{Name: "bad", Command: "npx", Headers: map[string]string{"A": "b"}, Enabled: true}},
		{{Name: "bad", Enabled: true}},
	} {
		if _, err := NewProfileManager("alice", servers, log); err == nil {
			t.Fatalf("mixed configuration accepted: %+v", servers)
		}
	}
}

func TestEnsureConnectedSelectsStdioWithoutURLValidation(t *testing.T) {
	helper := stdioHelperCommand(t)
	store := testStore(t)
	addTestUsers(t, store, "user_1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "user_1", Name: "local", Transport: TransportStdio, Command: helper.Path, Args: helper.Args[1:], Env: map[string]string{"OSWALD_TEST_STDIO_HELPER": "1"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	manager := newManager(store, config.NewLogger(config.LevelError))
	defer manager.Close()
	srv, err := manager.ensureConnected(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.tools) != 2 {
		t.Fatalf("stdio tools = %+v", srv.tools)
	}
	execution, err := manager.Execute(ctx, "user_1", "mcp__local__echo", map[string]interface{}{"text": "via-manager"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(execution.Content, "echo:via-manager") || execution.ServerName != "local" || execution.RemoteToolName != "echo" {
		t.Fatalf("stdio execution = %+v", execution)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}
