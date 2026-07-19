package builtin

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
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

func TestNewServiceWithPrivacyRegistersPrivacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	memory := usermemory.NewStore(path, log)
	defer memory.Close() // nolint:errcheck
	users := accountlinking.NewService(path, memory, nil, log)
	defer users.Close() // nolint:errcheck
	service, err := NewServiceWithPrivacy(users, memory, PrivacyDeps{Policy: config.RetentionPolicy{ForgottenContentGrace: 30 * 24 * time.Hour}, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range service.Definitions() {
		if definition.Name == "privacy" {
			return
		}
	}
	t.Fatal("privacy command was not registered")
}
