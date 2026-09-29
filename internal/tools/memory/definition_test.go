package memory

import (
	"slices"
	"strings"
	"testing"
)

func TestDefinitionContract(t *testing.T) {
	tool := Definition()
	if Name != "memory" || tool.Name != Name {
		t.Fatalf("tool identity = %+v", tool)
	}
	for _, guidance := range []string{"ALL your changes in ONE call", "atomically", "new sessions", "skill_manage", "session_search"} {
		if !strings.Contains(tool.Description, guidance) {
			t.Errorf("description missing %q", guidance)
		}
	}
	p := tool.Parameters
	if p.Type != "object" || !slices.Equal(p.Required, []string{"target"}) || len(p.Properties) != 6 {
		t.Fatalf("parameters = %+v", p)
	}
	if !slices.Equal(p.Properties["target"].Enum, []string{"memory", "user"}) ||
		!slices.Equal(p.Properties["action"].Enum, []string{"add", "replace", "remove"}) {
		t.Error("target/action choices changed")
	}
	for _, field := range []string{"content", "old_text", "new_text"} {
		if p.Properties[field].Type != "string" {
			t.Errorf("%s must be a string", field)
		}
	}
	if !strings.Contains(p.Properties["new_text"].Description, "'content' wins") ||
		!strings.Contains(p.Properties["old_text"].Description, "REQUIRED") {
		t.Error("single-operation alias or matching guidance missing")
	}
	ops := p.Properties["operations"]
	if ops.Type != "array" || ops.Items == nil {
		t.Fatalf("operations = %+v", ops)
	}
	if ops.Items.Type != "object" || !slices.Equal(ops.Items.Required, []string{"action"}) || len(ops.Items.Properties) != 4 {
		t.Fatalf("operation item = %+v", ops.Items)
	}
	if !slices.Equal(ops.Items.Properties["action"].Enum, []string{"add", "replace", "remove"}) {
		t.Error("batch action choices changed")
	}
	for _, field := range []string{"content", "new_text", "old_text"} {
		if ops.Items.Properties[field].Type != "string" {
			t.Errorf("batch %s must be a string", field)
		}
	}
}
