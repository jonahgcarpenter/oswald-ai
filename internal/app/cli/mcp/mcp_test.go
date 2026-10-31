package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpTestConfig = `providers:
  fake:
    api: "http://127.0.0.1:9999/v1"
model:
  provider: custom:fake
  default: fake/model
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
`

func mcpTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(mcpTestConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("soul"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func mcpRun(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestListEmptyAndSeeded(t *testing.T) {
	root := mcpTestRoot(t)
	if code, out, _ := mcpRun(t, root, "list"); code != 0 || !strings.Contains(out, "no MCP servers") {
		t.Fatalf("list empty = %d %q", code, out)
	}
	if code, _, stderr := mcpRun(t, root, "add", "docs", "--url", "https://example.com/mcp", "--header", "Authorization=Bearer x"); code != 0 {
		t.Fatalf("add = %d %s", code, stderr)
	}
	code, out, _ := mcpRun(t, root, "list")
	if code != 0 {
		t.Fatalf("list = %d", code)
	}
	for _, want := range []string{"docs", "streamable_http", "headers", "ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "Bearer") {
		t.Fatalf("list leaked a header value: %q", out)
	}
}

func TestAddEditRemoveRoundTrip(t *testing.T) {
	root := mcpTestRoot(t)
	if code, _, stderr := mcpRun(t, root, "add", "local", "--command", "npx", "--arg", "serve", "--cwd", "tools"); code != 0 {
		t.Fatalf("add stdio = %d %s", code, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	for _, want := range []string{"local:", "command: npx", "cwd: tools"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("config missing %q:\n%s", want, data)
		}
	}
	if code, _, stderr := mcpRun(t, root, "edit", "local", "--header", "X-Team=docs"); code == 0 {
		t.Fatalf("stdio header edit accepted: %s", stderr)
	}
	if code, _, stderr := mcpRun(t, root, "edit", "local", "--url", "https://example.com/mcp"); code != 0 {
		t.Fatalf("transport switch = %d %s", code, stderr)
	}
	data, _ = os.ReadFile(filepath.Join(root, "config.yaml"))
	if strings.Contains(string(data), "command: npx") || !strings.Contains(string(data), "https://example.com/mcp") {
		t.Fatalf("switch did not clear stdio keys:\n%s", data)
	}
	if code, out, _ := mcpRun(t, root, "remove", "local"); code != 0 || !strings.Contains(out, "tokens kept") {
		t.Fatalf("remove = %d %q", code, out)
	}
	if code, _, _ := mcpRun(t, root, "remove", "local"); code == 0 {
		t.Fatal("second remove accepted")
	}
}

func TestAddOAuthAutoMode(t *testing.T) {
	root := mcpTestRoot(t)
	args := []string{"add", "secure", "--url", "https://example.com/mcp",
		"--oauth-client-id", "cid", "--oauth-client-secret", "shh", "--oauth-flow", "device"}
	if code, _, stderr := mcpRun(t, root, args...); code != 0 {
		t.Fatalf("add oauth = %d %s", code, stderr)
	}
	code, out, _ := mcpRun(t, root, "list")
	if code != 0 || !strings.Contains(out, "oauth") || !strings.Contains(out, "no-token") {
		t.Fatalf("oauth list = %d %q", code, out)
	}
	if strings.Contains(out, "shh") {
		t.Fatalf("list leaked client secret: %q", out)
	}
}

func TestProfileScopeEnforced(t *testing.T) {
	root := mcpTestRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "profiles", "bob"), 0700); err != nil {
		t.Fatal(err)
	}
	bob := filepath.Join(root, "profiles", "bob")
	bobCfg := strings.Replace(mcpTestConfig, "api_port: 12345", "api_port: 12346", 1)
	if err := os.WriteFile(filepath.Join(bob, "config.yaml"), []byte(bobCfg), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", bob)
	if code, _, stderr := mcpRun(t, root, "add", "docs", "--url", "https://example.com/mcp"); code != 0 {
		t.Fatalf("env add = %d %s", code, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(bob, "config.yaml"))
	if !strings.Contains(string(data), "docs:") {
		t.Fatalf("add missed the bob profile:\n%s", data)
	}
	if code, _, _ := mcpRun(t, root, "list", "--profile", "alice"); code == 0 {
		// Reads tolerate another profile name but fail closed when it does
		// not exist.
		t.Fatal("list of missing profile accepted")
	}
	// Reads of an existing profile are visible cross-profile…
	if code, out, _ := mcpRun(t, root, "list", "--profile", "default"); code != 0 {
		t.Fatalf("cross-profile list rejected: %d %q", code, out)
	}
	// …but mutations stay fenced to the calling scope.
	if code, _, _ := mcpRun(t, root, "add", "evil", "--profile", "default", "--url", "https://example.com/mcp"); code == 0 {
		t.Fatal("cross-profile add accepted")
	}
}

func TestTestCommandAgainstFakeServer(t *testing.T) {
	muxServer := gomcp.NewServer(&gomcp.Implementation{Name: "fake", Version: "1"}, nil)
	gomcp.AddTool(muxServer, &gomcp.Tool{Name: "echo", Description: "Echo"},
		func(context.Context, *gomcp.CallToolRequest, any) (*gomcp.CallToolResult, any, error) {
			return &gomcp.CallToolResult{}, nil, nil
		})
	httpServer := httptest.NewServer(gomcp.NewSSEHandler(func(*http.Request) *gomcp.Server { return muxServer }, nil))
	defer httpServer.Close()
	root := mcpTestRoot(t)
	if code, _, stderr := mcpRun(t, root, "add", "fake", "--url", httpServer.URL, "--transport", "sse"); code != 0 {
		t.Fatalf("add = %d %s", code, stderr)
	}
	code, out, _ := mcpRun(t, root, "test", "fake", "--timeout", "10s")
	if code != 0 {
		t.Fatalf("test = %d %q", code, out)
	}
	for _, want := range []string{"ok fake:", "1 tools", "echo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("test missing %q: %q", want, out)
		}
	}
}

func TestTestCommandDisabledAndUnknown(t *testing.T) {
	root := mcpTestRoot(t)
	if code, _, stderr := mcpRun(t, root, "add", "off", "--url", "https://example.com/mcp", "--disable"); code != 0 {
		t.Fatalf("add = %d %s", code, stderr)
	}
	code, out, _ := mcpRun(t, root, "test", "off")
	if code != 0 || !strings.Contains(out, "disabled") {
		t.Fatalf("disabled test = %d %q", code, out)
	}
	if code, _, _ := mcpRun(t, root, "test", "ghost"); code == 0 {
		t.Fatal("unknown server test accepted")
	}
	if code, _, _ := mcpRun(t, root, "test", "off", "--timeout", "bogus"); code == 0 {
		t.Fatal("bad timeout accepted")
	}
}

func TestRemoveWithTokens(t *testing.T) {
	root := mcpTestRoot(t)
	if code, _, stderr := mcpRun(t, root, "add", "docs", "--url", "https://example.com/mcp"); code != 0 {
		t.Fatalf("add = %d %s", code, stderr)
	}
	tokDir := filepath.Join(root, "mcp-tokens")
	if err := os.MkdirAll(tokDir, 0700); err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(tokDir, "docs.json")
	if err := os.WriteFile(tokFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := mcpRun(t, root, "remove", "docs"); code != 0 || !strings.Contains(out, "tokens kept") {
		t.Fatalf("remove = %d %q", code, out)
	}
	if _, err := os.Stat(tokFile); err != nil {
		t.Fatal("default remove deleted tokens")
	}
	if code, _, stderr := mcpRun(t, root, "add", "docs", "--url", "https://example.com/mcp"); code != 0 {
		t.Fatalf("re-add = %d %s", code, stderr)
	}
	if code, out, _ := mcpRun(t, root, "remove", "docs", "--with-tokens"); code != 0 || !strings.Contains(out, "and its tokens") {
		t.Fatalf("remove with tokens = %d %q", code, out)
	}
	if _, err := os.Stat(tokFile); !os.IsNotExist(err) {
		t.Fatal("tokens survived --with-tokens")
	}
}
