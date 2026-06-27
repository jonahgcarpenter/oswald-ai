package mcp

import "testing"

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
}

func TestSchemaTypeSkipsNullUnionType(t *testing.T) {
	if got := schemaType([]any{"null", "integer"}); got != "integer" {
		t.Fatalf("schemaType union = %q, want integer", got)
	}
}
