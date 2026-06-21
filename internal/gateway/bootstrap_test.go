package gateway

import (
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestNewServicesFromConfigEnablesConfiguredGateways(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	dir := t.TempDir()
	links := accountlinking.NewService(filepath.Join(dir, "links.json"), usermemory.NewStore(filepath.Join(dir, "users"), log), log)

	services, err := NewServicesFromConfig(&config.Config{Port: "8000"}, links, commands.NewRouter(), log)
	if err != nil {
		t.Fatalf("default services: %v", err)
	}
	if serviceNames(services) != "Websocket" {
		t.Fatalf("unexpected default services %q", serviceNames(services))
	}

	services, err = NewServicesFromConfig(&config.Config{Port: "8000", DiscordToken: "token", BlueBubblesURL: "http://bb", BlueBubblesPassword: "pw"}, links, commands.NewRouter(), log)
	if err != nil {
		t.Fatalf("configured services: %v", err)
	}
	if serviceNames(services) != "Websocket, Discord, iMessage" {
		t.Fatalf("unexpected configured services %q", serviceNames(services))
	}
}
