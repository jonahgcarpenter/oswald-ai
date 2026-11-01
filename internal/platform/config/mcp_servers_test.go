package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mcpTestConfig = `providers:
  fake:
    api: http://localhost:9999/v1
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
	return root
}

func mcpServerDoc(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	start := strings.Index(text, "mcp_servers:")
	if start < 0 {
		t.Fatalf("no mcp_servers block in:\n%s", text)
	}
	return text
}

func TestAddMCPServerHTTP(t *testing.T) {
	root := mcpTestRoot(t)
	url := "https://example.com/mcp"
	err := AddMCPServer(root, "default", "docs", MCPServerSpec{
		URL: &url, Transport: strptr("sse"),
		HeadersSet: map[string]string{"Authorization": "Bearer s3cret"},
	})
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	doc := mcpServerDoc(t, root, "docs")
	for _, want := range []string{"docs:", "https://example.com/mcp", "sse", "Authorization"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("doc missing %q:\n%s", want, doc)
		}
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("staged write does not validate: %v", err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Transport != "sse" {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
}

func TestAddMCPServerStdio(t *testing.T) {
	root := mcpTestRoot(t)
	cmd, cwd := "npx", "tools"
	err := AddMCPServer(root, "default", "local", MCPServerSpec{
		Command: &cmd, Cwd: &cwd, EnvSet: map[string]string{"TOKEN": "x"},
	})
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("staged write does not validate: %v", err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Transport != "stdio" {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
}

// A fresh `oswald setup` root declares a bare `mcp_servers:` key with a null
// value. The first add must replace it with a mapping instead of appending to
// the scalar, which would serialize to nothing and report false success.
func TestAddMCPServerToBareSection(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	doc := mcpTestConfig + "mcp_servers:\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	url := "https://example.com/mcp"
	if err := AddMCPServer(root, "default", "docs", MCPServerSpec{URL: &url}); err != nil {
		t.Fatalf("add failed: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "docs:") {
		t.Fatalf("first add to bare section did not persist:\n%s", after)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatalf("staged write does not validate: %v", err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "docs" {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
}

func TestAddMCPServerRejects(t *testing.T) {
	url := "https://example.com/mcp"
	cmd := "npx"
	oauth := "oauth"
	specs := map[string]MCPServerSpec{
		"no-transport":    {},
		"both-transports": {URL: &url, Command: &cmd},
		"oauth-stdio":     {Command: &cmd, Auth: &oauth},
	}
	names := map[string]string{
		"no-transport": "bare", "both-transports": "both", "oauth-stdio": "so",
	}
	for key, spec := range specs {
		root := mcpTestRoot(t)
		before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
		if err := AddMCPServer(root, "default", names[key], spec); err == nil {
			t.Fatalf("%s accepted", key)
		}
		after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
		if string(before) != string(after) {
			t.Fatalf("%s mutated the file", key)
		}
	}
	root := mcpTestRoot(t)
	if err := AddMCPServer(root, "default", "Bad Name!", MCPServerSpec{URL: &url}); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestAddMCPServerRejectsDuplicate(t *testing.T) {
	root := mcpTestRoot(t)
	url := "https://example.com/mcp"
	if err := AddMCPServer(root, "default", "dup", MCPServerSpec{URL: &url}); err != nil {
		t.Fatalf("setup add failed: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err := AddMCPServer(root, "default", "dup", MCPServerSpec{URL: &url}); err == nil {
		t.Fatal("duplicate accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if string(before) != string(after) {
		t.Fatal("duplicate mutated the file")
	}
}

func TestUpdateMCPServerPatch(t *testing.T) {
	root := mcpTestRoot(t)
	url := "https://example.com/mcp"
	if err := AddMCPServer(root, "default", "docs", MCPServerSpec{URL: &url}); err != nil {
		t.Fatal(err)
	}
	label := "Docs"
	if err := UpdateMCPServer(root, "default", "docs", MCPServerSpec{
		Transport: strptr("sse"), HeadersSet: map[string]string{"X-Team": "docs"},
	}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.MCPServers[0]
	if got.Transport != "sse" || got.Headers["X-Team"] != "docs" || got.URL != url {
		t.Fatalf("merged = %+v", got)
	}
	_ = label
	// Transport switch clears the other side.
	command := "npx"
	if err := UpdateMCPServer(root, "default", "docs", MCPServerSpec{Command: &command}); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	cfg, err = LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	got = cfg.MCPServers[0]
	if got.URL != "" || got.Transport != "stdio" || len(got.Headers) != 0 {
		t.Fatalf("after switch = %+v", got)
	}
	if err := UpdateMCPServer(root, "default", "missing", MCPServerSpec{}); err == nil {
		t.Fatal("update of missing server accepted")
	}
}

func TestUpdateMCPServerOAuth(t *testing.T) {
	root := mcpTestRoot(t)
	url := "https://example.com/mcp"
	if err := AddMCPServer(root, "default", "docs", MCPServerSpec{URL: &url}); err != nil {
		t.Fatal(err)
	}
	secret, flow := "shh", "device"
	if err := UpdateMCPServer(root, "default", "docs", MCPServerSpec{
		OAuthClientID: strptr("cid"), OAuthClientSecret: &secret, OAuthFlow: &flow,
	}); err != nil {
		t.Fatalf("oauth update failed: %v", err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.MCPServers[0]
	if got.Auth != "oauth" || got.OAuth.ClientID != "cid" || got.OAuth.Flow != "device" {
		t.Fatalf("oauth = %+v", got.OAuth)
	}
	if err := UpdateMCPServer(root, "default", "docs", MCPServerSpec{ClearOAuth: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers[0].Auth != "oauth" || cfg.MCPServers[0].OAuth.ClientID != "" {
		t.Fatalf("clear oauth = %+v", cfg.MCPServers[0].OAuth)
	}
}

func TestRemoveMCPServer(t *testing.T) {
	root := mcpTestRoot(t)
	url := "https://example.com/mcp"
	if err := AddMCPServer(root, "default", "docs", MCPServerSpec{URL: &url}); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveMCPServer(root, "default", "docs")
	if err != nil || !removed {
		t.Fatalf("remove = %v %v", removed, err)
	}
	removed, err = RemoveMCPServer(root, "default", "docs")
	if err != nil || removed {
		t.Fatalf("second remove = %v %v", removed, err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil || len(cfg.MCPServers) != 0 {
		t.Fatalf("servers = %+v %v", cfg.MCPServers, err)
	}
}

func strptr(value string) *string { return &value }
