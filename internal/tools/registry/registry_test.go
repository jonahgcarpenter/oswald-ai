package registry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestRegistryLoadsMarkdownAndExecutesHandler(t *testing.T) {
	dir := t.TempDir()
	definition := `# test.echo

## Description

Echo a value.

## Parameters

| Name | Type | Required | Description |
| ---- | ---- | -------- | ----------- |
| text | string | yes | Text to echo |
`
	if err := os.WriteFile(filepath.Join(dir, "echo.md"), []byte(definition), 0o644); err != nil {
		t.Fatalf("write definition: %v", err)
	}

	reg, err := NewFromDirectory(dir, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	if reg.Count() != 1 || reg.Names()[0] != "test.echo" {
		t.Fatalf("unexpected registry names: %+v", reg.Names())
	}
	if err := reg.RegisterHandler("missing", func(context.Context, map[string]interface{}) (string, error) { return "", nil }); err == nil {
		t.Fatal("expected unknown handler registration error")
	}
	if err := reg.RegisterHandler("test.echo", func(_ context.Context, args map[string]interface{}) (string, error) {
		return args["text"].(string), nil
	}); err != nil {
		t.Fatalf("register handler: %v", err)
	}

	got, err := reg.Execute(context.Background(), "test.echo", map[string]interface{}{"text": "hello"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != "hello" {
		t.Fatalf("got %q, want hello", got)
	}

	tools := reg.LLMTools()
	if len(tools) != 1 || tools[0].Function.Name != "test.echo" {
		t.Fatalf("unexpected LLM tools: %+v", tools)
	}
	if len(tools[0].Function.Parameters.Required) != 1 || tools[0].Function.Parameters.Required[0] != "text" {
		t.Fatalf("unexpected required params: %+v", tools[0].Function.Parameters.Required)
	}
}

func TestRegistryUnknownToolListsMatchingPrefixHandlers(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	registerTestTool(t, reg, "files.read")
	registerTestTool(t, reg, "files.search")
	registerTestTool(t, reg, "files.list")
	registerTestTool(t, reg, "files.delete")
	registerTestTool(t, reg, "web.search")

	_, err := reg.Execute(context.Background(), "files.missing", nil)
	if err == nil {
		t.Fatal("expected unknown tool error")
	}
	want := `no handler registered for tool "files.missing"; available tools in prefix "files": files.delete, files.list, files.read, files.search`
	if err.Error() != want {
		t.Fatalf("unexpected error %q, want %q", err.Error(), want)
	}
}

func TestRegistryUnknownToolWithoutPrefixListsAllHandlers(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	registerTestTool(t, reg, "files.read")
	registerTestTool(t, reg, "files.delete")
	registerTestTool(t, reg, "web.search")

	_, err := reg.Execute(context.Background(), "delete", nil)
	if err == nil {
		t.Fatal("expected unknown tool error")
	}
	want := `no handler registered for tool "delete"; available tools: files.delete, files.read, web.search`
	if err.Error() != want {
		t.Fatalf("unexpected error %q, want %q", err.Error(), want)
	}
}

func TestRegistryUnknownToolWithEmptyPrefixMatchListsNone(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	registerTestTool(t, reg, "files.read")
	registerTestTool(t, reg, "web.search")

	_, err := reg.Execute(context.Background(), "missing.write", nil)
	if err == nil {
		t.Fatal("expected unknown tool error")
	}
	want := `no handler registered for tool "missing.write"; available tools in prefix "missing": none`
	if err.Error() != want {
		t.Fatalf("unexpected error %q, want %q", err.Error(), want)
	}
}

func registerTestTool(t *testing.T, reg *Registry, name string) {
	t.Helper()
	if err := reg.RegisterTool(Spec{Name: name, Description: strings.TrimPrefix(name, "test.")}, func(context.Context, map[string]interface{}) (string, error) {
		return "ok", nil
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func TestRegistryMCPVisibilityAndCatalogOrdering(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	if err := reg.RegisterSpec(Spec{Name: "builtin.tool", Description: "Builtin", Source: ToolSourceBuiltin}); err != nil {
		t.Fatalf("register builtin: %v", err)
	}
	if err := reg.RegisterSpec(Spec{Name: "github.get_issue", Description: "Get issue", Source: ToolSourceMCP, Server: "github"}); err != nil {
		t.Fatalf("register mcp: %v", err)
	}
	if err := reg.RegisterSpec(Spec{Name: "github.get_repo", Description: "Get repo", Source: ToolSourceMCP, Server: "github"}); err != nil {
		t.Fatalf("register second mcp: %v", err)
	}

	tools := reg.LLMTools()
	if len(tools) != 1 || tools[0].Function.Name != "builtin.tool" {
		t.Fatalf("expected only builtin by default, got %+v", tools)
	}

	tools = reg.LLMToolsForVisibility(ToolVisibility{ExposedMCPTools: map[string]bool{"github.get_issue": true}})
	if len(tools) != 2 || tools[0].Function.Name != "builtin.tool" || tools[1].Function.Name != "github.get_issue" {
		t.Fatalf("unexpected visible tools: %+v", tools)
	}
	if tools[1].Function.Description != "Github MCP tool: Get issue" {
		t.Fatalf("unexpected MCP description %q", tools[1].Function.Description)
	}

	builtin := reg.BuiltinCatalog()
	if len(builtin) != 1 || builtin[0].Name != "builtin.tool" {
		t.Fatalf("unexpected builtin catalog: %+v", builtin)
	}
	mcp := reg.CatalogBySource(ToolSourceMCP)
	if len(mcp) != 2 || mcp[0].Name != "github.get_issue" || mcp[1].Name != "github.get_repo" {
		t.Fatalf("unexpected mcp catalog: %+v", mcp)
	}
}

func TestParseToolMarkdownRejectsMissingSections(t *testing.T) {
	if _, err := parseToolMarkdown("# missing.description\n\n## Parameters\n\n| Name | Type | Required | Description |\n| ---- | ---- | -------- | ----------- |"); err == nil {
		t.Fatal("expected missing description error")
	}
	if _, err := parseToolMarkdown("# missing.params\n\n## Description\n\nDescription"); err == nil {
		t.Fatal("expected missing parameters error")
	}
}
