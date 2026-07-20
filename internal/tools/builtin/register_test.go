package builtin

import (
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func TestRegisterIncludesCurrentTimeTool(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, nil, "", log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	if !reg.HasHandler("time.current") {
		t.Fatal("time.current handler was not registered")
	}

	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != "time.current" {
			continue
		}
		if len(entry.Parameters) != 1 || entry.Parameters[0].Name != "timezone" || entry.Parameters[0].Type != "string" || !entry.Parameters[0].Required {
			t.Fatalf("unexpected time.current parameters: %+v", entry.Parameters)
		}
		return
	}
	t.Fatal("time.current schema was not loaded")
}

func TestRegisterIncludesTranscriptSearchTool(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatalf("load tool definitions: %v", err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, nil, "", log); err != nil {
		t.Fatalf("register builtin handlers: %v", err)
	}
	if !reg.HasHandler("transcript.search") {
		t.Fatal("transcript.search handler was not registered")
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != "transcript.search" {
			continue
		}
		if len(entry.Parameters) != 2 || entry.Parameters[0].Name != "query" || entry.Parameters[0].Type != "string" || !entry.Parameters[0].Required || entry.Parameters[1].Name != "limit" || entry.Parameters[1].Type != "integer" || entry.Parameters[1].Required {
			t.Fatalf("unexpected transcript.search parameters: %+v", entry.Parameters)
		}
		return
	}
	t.Fatal("transcript.search schema was not loaded")
}

func TestRegisterMemoryForgetUsesExactRequiredID(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, nil, "", log); err != nil {
		t.Fatal(err)
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != "memory.forget" {
			continue
		}
		if len(entry.Parameters) != 1 || entry.Parameters[0].Name != "memory_id" || entry.Parameters[0].Type != "integer" || !entry.Parameters[0].Required {
			t.Fatalf("unexpected memory.forget parameters: %+v", entry.Parameters)
		}
		return
	}
	t.Fatal("memory.forget schema was not loaded")
}

func TestRegisterDeploymentMemoryIsDefaultVisibleWithOptionalToolCallID(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	reg, err := registry.NewFromDirectory(filepath.Join("..", "..", "..", "data", "tools"), log)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(reg, &config.Config{}, nil, nil, nil, nil, "", log); err != nil {
		t.Fatal(err)
	}
	foundVisible := false
	for _, tool := range reg.LLMTools() {
		if tool.Function.Name == "deployment_memory.propose" {
			foundVisible = true
		}
	}
	if !foundVisible {
		t.Fatal("deployment_memory.propose is not default-visible")
	}
	for _, entry := range reg.BuiltinCatalog() {
		if entry.Name != "deployment_memory.propose" {
			continue
		}
		for _, parameter := range entry.Parameters {
			if parameter.Name == "source_tool_call_id" && parameter.Required {
				t.Fatal("source_tool_call_id must be optional")
			}
		}
		return
	}
	t.Fatal("deployment_memory.propose schema was not loaded")
}
