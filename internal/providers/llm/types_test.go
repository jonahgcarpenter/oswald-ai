package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequiredEmittedExplicitly(t *testing.T) {
	top := ToolParameters{Type: "object", Properties: map[string]ToolParameterProperty{
		"query": {Type: "string"},
	}}
	encoded, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"required":[]`) {
		t.Fatalf("top-level required omitted, got %s", encoded)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	required, ok := decoded["required"].([]interface{})
	if !ok || len(required) != 0 {
		t.Fatalf("top-level required is not an empty array: %v", decoded["required"])
	}
	props, _ := decoded["properties"].(map[string]interface{})
	entry, _ := props["query"].(map[string]interface{})
	nested, ok := entry["required"].([]interface{})
	if !ok || len(nested) != 0 {
		t.Fatalf("nested required is not an empty array: %v", entry["required"])
	}

	filled := ToolParameters{Type: "object", Required: []string{"query"}}
	encoded, err = json.Marshal(filled)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"required":["query"]`) {
		t.Fatalf("non-empty required lost, got %s", encoded)
	}
}
