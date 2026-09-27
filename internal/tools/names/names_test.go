package names

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestBuiltinNamesMatchStableSchemaContract(t *testing.T) {
	contracts := []struct{ name, literal string }{
		{Memory, "memory"},
		{WebSearch, "web_search"},
		{SessionTranscriptSearch, "session_transcript_search"},
		{ComfyUITextToImage, "comfyui.text_to_image"},
		{ComfyUIImageToImage, "comfyui.image_to_image"},
	}
	want := make([]string, 0, len(contracts))
	for _, contract := range contracts {
		if contract.name != contract.literal {
			t.Errorf("builtin name = %q, want stable literal %q", contract.name, contract.literal)
		}
		want = append(want, contract.name)
	}
	slices.Sort(want)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	got := reg.Names()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("loaded schema names = %q, want exactly %q", got, want)
	}
}

func TestMemorySchemaAdvertisesBatchAndAlias(t *testing.T) {
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name != Memory {
			continue
		}
		schema := tool.Function.Parameters
		if !slices.Equal(schema.Required, []string{"target"}) || len(schema.Properties) != 6 {
			t.Fatalf("memory parameters: %+v", schema)
		}
		ops := schema.Properties["operations"]
		if ops.Type != "array" || ops.Items == nil || !slices.Equal(ops.Items.Required, []string{"action"}) || len(ops.Items.Properties) != 4 {
			t.Fatalf("memory batch schema: %+v", ops)
		}
		if !strings.Contains(tool.Function.Description, "ALL your changes in ONE call") || !strings.Contains(schema.Properties["new_text"].Description, "'content' wins") {
			t.Fatalf("memory guidance missing: %q %+v", tool.Function.Description, schema.Properties["new_text"])
		}
		return
	}
	t.Fatal("memory tool missing")
}
