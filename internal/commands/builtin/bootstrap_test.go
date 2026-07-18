package builtin

import (
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestNewServiceAlwaysRegistersReset(t *testing.T) {
	memory := usermemory.NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer memory.Close() // nolint:errcheck
	service, err := NewService(nil, memory)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range service.Definitions() {
		if definition.Name == "reset" {
			return
		}
	}
	t.Fatal("reset command was not registered")
}
