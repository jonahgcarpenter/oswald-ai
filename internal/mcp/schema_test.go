package mcp

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSchemaToParamsNormalizesObjectSchema(t *testing.T) {
	params, err := schemaToParams(map[string]interface{}{
		"type":     []interface{}{"null", "object"},
		"required": []interface{}{"owner", "private"},
		"properties": map[string]interface{}{
			"owner": map[string]interface{}{
				"type":        "string",
				"description": " repo owner ",
			},
			"private": map[string]interface{}{
				"type": "boolean",
			},
			"visibility": map[string]interface{}{
				"enum": []interface{}{"public", "private"},
			},
		},
	})
	if err != nil {
		t.Fatalf("schemaToParams returned error: %v", err)
	}

	byName := make(map[string]ParamSpec, len(params))
	for _, param := range params {
		byName[param.Name] = param
	}
	if byName["owner"].Type != "string" || !byName["owner"].Required || byName["owner"].Description != "repo owner" {
		t.Fatalf("owner param = %+v", byName["owner"])
	}
	if byName["private"].Type != "boolean" || !byName["private"].Required {
		t.Fatalf("private param = %+v", byName["private"])
	}
	if byName["visibility"].Type != "string" || len(byName["visibility"].Enum) != 2 {
		t.Fatalf("visibility param = %+v", byName["visibility"])
	}
}

func TestSchemaToParamsRejectsUnsupportedSchemas(t *testing.T) {
	if _, err := schemaToParams(map[string]interface{}{"type": "array"}); err == nil {
		t.Fatal("array top-level schema returned nil error")
	}
	if _, err := schemaToParams(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bad": map[string]interface{}{"type": "symbol"},
		},
	}); err == nil {
		t.Fatal("unsupported property type returned nil error")
	}
	for _, name := range []string{"bad name", "9starts_with_number", "dot.name", "naive-é", strings.Repeat("a", maxProviderIdentifierBytes+1)} {
		if _, err := schemaToParams(map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{name: map[string]interface{}{"type": "string"}},
		}); err == nil {
			t.Fatalf("invalid property name %q returned nil error", name)
		}
	}
}

func TestToolSpecRejectsInvalidRemoteNamesBeforeConstruction(t *testing.T) {
	for _, name := range []string{"bad name", "9tool", "nested.tool", "tool/escape", strings.Repeat("a", maxProviderIdentifierBytes)} {
		_, err := toolSpec(ServerConfig{Name: "server_name"}, &gomcp.Tool{Name: name, InputSchema: map[string]interface{}{"type": "object"}}, nil, config.NewLogger(config.LevelError))
		if err == nil {
			t.Fatalf("invalid remote tool name %q accepted", name)
		}
	}
	if _, err := toolSpec(ServerConfig{Name: "home"}, &gomcp.Tool{Name: "read_status", InputSchema: map[string]interface{}{"type": "object"}}, nil, config.NewLogger(config.LevelError)); err != nil {
		t.Fatalf("valid remote tool name rejected: %v", err)
	}
}

func TestSchemaTypeSkipsNullUnionType(t *testing.T) {
	if got := schemaType([]any{"null", "integer"}); got != "integer" {
		t.Fatalf("schemaType union = %q, want integer", got)
	}
}
