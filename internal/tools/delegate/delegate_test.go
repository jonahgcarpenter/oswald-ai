package delegate

import (
	"strings"
	"testing"
)

func TestParseSpawnSingle(t *testing.T) {
	call, err := ParseArgs(map[string]interface{}{"goal": "Research X", "context": "Focus on Y"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if call.Action != "spawn" || len(call.Tasks) != 1 || call.Tasks[0].Goal != "Research X" || call.Role != "leaf" {
		t.Fatalf("call = %+v", call)
	}
}

func TestParseSpawnBatch(t *testing.T) {
	call, err := ParseArgs(map[string]interface{}{
		"tasks": []interface{}{
			map[string]interface{}{"goal": "A", "context": "a", "output_schema": map[string]interface{}{"type": "object"}},
			map[string]interface{}{"goal": "B", "images": []interface{}{"/tmp/x.png"}},
		},
		"role": "orchestrator",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(call.Tasks) != 2 || call.Role != "orchestrator" {
		t.Fatalf("call = %+v", call)
	}
	if call.Tasks[0].OutputSchema["type"] != "object" || len(call.Tasks[1].Images) != 1 {
		t.Fatalf("tasks = %+v", call.Tasks)
	}
}

func TestParseRejects(t *testing.T) {
	big := make([]interface{}, MaxTasksPerCall+1)
	for i := range big {
		big[i] = map[string]interface{}{"goal": "x"}
	}
	images := make([]interface{}, MaxImagesPerTask+1)
	for i := range images {
		images[i] = "x.png"
	}
	for _, args := range []map[string]interface{}{
		{},
		{"goal": "  "},
		{"goal": "x", "tasks": []interface{}{map[string]interface{}{"goal": "y"}}},
		{"tasks": "nope"},
		{"tasks": []interface{}{}},
		{"tasks": big},
		{"tasks": []interface{}{map[string]interface{}{}}},
		{"tasks": []interface{}{map[string]interface{}{"goal": "x", "output_schema": []interface{}{}}}},
		{"tasks": []interface{}{map[string]interface{}{"goal": "x", "images": images}}},
		{"goal": "x", "images": "nope"},
		{"action": "dance"},
		{"action": "steer"},
		{"action": "steer", "subagent_id": "s-1"},
		{"action": "stop"},
		{"role": "manager"},
	} {
		if _, err := ParseArgs(args); err == nil {
			t.Fatalf("accepted %+v", args)
		}
	}
}

func TestParseControl(t *testing.T) {
	call, err := ParseArgs(map[string]interface{}{"action": "list"})
	if err != nil || call.Action != "list" || len(call.Tasks) != 0 {
		t.Fatalf("call = %+v %v", call, err)
	}
	call, err = ParseArgs(map[string]interface{}{"action": "steer", "subagent_id": " s-1 ", "message": "go left"})
	if err != nil || call.SubagentID != "s-1" || call.Message != "go left" {
		t.Fatalf("call = %+v %v", call, err)
	}
	if _, err := ParseArgs(map[string]interface{}{"action": "steer", "subagent_id": "s-1", "message": "  "}); err == nil {
		t.Fatal("empty steer message accepted")
	}
}

func TestDefinitionContract(t *testing.T) {
	def := Definition()
	if def.Name != Name || def.Parameters.Type != "object" {
		t.Fatalf("definition = %+v", def)
	}
	for _, key := range []string{"action", "goal", "context", "tasks", "output_schema", "images", "role", "subagent_id", "message"} {
		if _, ok := def.Parameters.Properties[key]; !ok {
			t.Fatalf("missing property %q", key)
		}
	}
	if got := def.Parameters.Properties["action"].Enum; len(got) != 4 {
		t.Fatalf("action enum = %v", got)
	}
	for _, want := range []string{"goal", "context", "TASK i/N", "subagent_id"} {
		if !strings.Contains(def.Description, want) {
			t.Fatalf("description missing %q", want)
		}
	}
}
