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
	if err := Register(reg, &config.Config{}, nil, nil, nil, "", log); err != nil {
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
