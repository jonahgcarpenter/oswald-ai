package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadMCPConfig(t *testing.T, mcpYAML string) *Config {
	t.Helper()
	root := profileConfigFixture(t, syntheticProfileConfig+"mcp_servers:\n"+mcpYAML)
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func loadMCPConfigErr(t *testing.T, mcpYAML string) error {
	t.Helper()
	root := profileConfigFixture(t, syntheticProfileConfig+"mcp_servers:\n"+mcpYAML)
	_, err := LoadProfiles(root)
	if err == nil {
		t.Fatal("invalid MCP configuration accepted")
	}
	return err
}

func TestMCPHTTPServerDefaults(t *testing.T) {
	cfg := loadMCPConfig(t, "    notes:\n      url: https://example.com/mcp\n")
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
	server := cfg.MCPServers[0]
	if server.Name != "notes" || server.URL != "https://example.com/mcp" || server.Transport != "streamable_http" || !server.Enabled {
		t.Fatalf("server = %+v", server)
	}
	if server.Command != "" || len(server.Args) != 0 || server.Cwd != "" || len(server.Env) != 0 || len(server.Include) != 0 || len(server.Exclude) != 0 {
		t.Fatalf("unexpected stdio/filter state: %+v", server)
	}
}

func TestMCPHTTPSSEAndFilters(t *testing.T) {
	cfg := loadMCPConfig(t, "    bbb:\n      url: http://localhost:8080/sse\n      transport: sse\n      headers:\n        Authorization: Bearer token\n      tools:\n        include: [read, search, read]\n        exclude: [delete]\n    aaa:\n      url: https://example.com/mcp\n")
	if len(cfg.MCPServers) != 2 || cfg.MCPServers[0].Name != "aaa" || cfg.MCPServers[1].Name != "bbb" {
		t.Fatalf("servers not sorted: %+v", cfg.MCPServers)
	}
	bbb := cfg.MCPServers[1]
	if bbb.Transport != "sse" || bbb.Headers["Authorization"] != "Bearer token" {
		t.Fatalf("server = %+v", bbb)
	}
	if len(bbb.Include) != 2 || bbb.Include[0] != "read" || bbb.Include[1] != "search" || len(bbb.Exclude) != 1 || bbb.Exclude[0] != "delete" {
		t.Fatalf("filters = %+v %+v", bbb.Include, bbb.Exclude)
	}
}

func TestMCPStdioServerResolution(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig+"mcp_servers:\n    notes:\n      command: npx\n      args: [\"-y\", \"@example/notes-server\"]\n      cwd: work\n      env:\n        LOG_LEVEL: info\n        API_KEY: ${PROFILE_MCP_API_KEY}\n      tools:\n        exclude: [edit]\n")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("PROFILE_MCP_API_KEY=secret-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("servers = %+v", cfg.MCPServers)
	}
	server := cfg.MCPServers[0]
	if server.Command != "npx" || server.Transport != "stdio" || !server.Enabled {
		t.Fatalf("server = %+v", server)
	}
	if len(server.Args) != 2 || server.Args[1] != "@example/notes-server" {
		t.Fatalf("args = %+v", server.Args)
	}
	if server.Cwd != filepath.Join(root, "work") {
		t.Fatalf("cwd = %q", server.Cwd)
	}
	if server.Env["LOG_LEVEL"] != "info" || server.Env["API_KEY"] != "secret-token" {
		t.Fatalf("env = %+v", server.Env)
	}
	if len(server.Exclude) != 1 || server.Exclude[0] != "edit" {
		t.Fatalf("exclude = %+v", server.Exclude)
	}
}

func TestMCPLegacyNestedKeyRejected(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig+"mcp:\n  servers:\n    bad:\n      url: https://example.com/mcp\n")
	if _, err := LoadProfiles(root); err == nil {
		t.Fatal("legacy nested mcp key accepted")
	} else if code := configErrorCode(err); code != "config_yaml_invalid" {
		t.Fatalf("error code = %q", code)
	}
}

func TestMCPServerValidation(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"neither url nor command", "    bad:\n      transport: sse\n"},
		{"url and command", "    bad:\n      url: https://example.com/mcp\n      command: npx\n"},
		{"transport with command", "    bad:\n      command: npx\n      transport: sse\n"},
		{"args with url", "    bad:\n      url: https://example.com/mcp\n      args: [x]\n"},
		{"cwd with url", "    bad:\n      url: https://example.com/mcp\n      cwd: .\n"},
		{"env with url", "    bad:\n      url: https://example.com/mcp\n      env:\n        A: b\n"},
		{"headers with command", "    bad:\n      command: npx\n      headers:\n        A: b\n"},
		{"bad transport", "    bad:\n      url: https://example.com/mcp\n      transport: websocket\n"},
		{"ftp scheme", "    bad:\n      url: ftp://example.com/mcp\n"},
		{"userinfo", "    bad:\n      url: https://user@example.com/mcp\n"},
		{"legacy description", "    bad:\n      url: https://example.com/mcp\n      description: old field\n"},
		{"reserved soul", "    soul:\n      url: https://example.com/mcp\n"},
		{"bad server name", "    Bad:\n      url: https://example.com/mcp\n"},
		{"bad filter name", "    bad:\n      url: https://example.com/mcp\n      tools:\n        include: [has space]\n"},
		{"cwd escape", "    bad:\n      command: npx\n      cwd: ../outside\n"},
		{"too many args", "    bad:\n      command: npx\n      args: [" + strings.Repeat("\"x\",", 33) + "\"x\"]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := loadMCPConfigErr(t, tc.yaml)
			if code := configErrorCode(err); code != "config_mcp_invalid" && code != "config_yaml_invalid" {
				t.Fatalf("error code = %q (%v)", code, err)
			}
		})
	}
}

func configErrorCode(err error) string {
	if classified, ok := AsConfigError(err); ok {
		return classified.Code
	}
	return ""
}
