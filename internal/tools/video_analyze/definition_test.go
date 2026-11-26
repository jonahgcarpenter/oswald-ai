package video_analyze

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
)

func TestDefinitionContract(t *testing.T) {
	def := Definition()
	if def.Name != Name || Name != "video_analyze" {
		t.Fatalf("name = %q", def.Name)
	}
	var _ llm.ToolDefinition = def
	props := def.Parameters.Properties
	for _, key := range []string{"source", "question", "frames", "timestamps"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("missing property %q", key)
		}
	}
	for _, key := range def.Parameters.Required {
		if key != "source" && key != "question" {
			t.Fatalf("unexpected required %q", key)
		}
	}
	if len(def.Parameters.Required) != 2 {
		t.Fatalf("required = %v", def.Parameters.Required)
	}
	if got := *props["frames"].Maximum; got != MaxFrames {
		t.Fatalf("frames max = %v", got)
	}
	if got := *props["timestamps"].MaxItems; got != MaxFrames {
		t.Fatalf("timestamps max = %v", got)
	}
}
