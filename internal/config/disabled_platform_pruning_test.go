package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDisabledPlatformCredentialIsNotResolved(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-model-key")
	t.Setenv("PROFILE_TEST_DISCORD_TOKEN", "synthetic-discord-token")
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(disabledPlatformConfig), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	raw := document.Content[0]
	pruned, err := pruneDisabledPlatforms(raw, profileEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := resolveDocument(pruned, profileEnvironment{})
	if err != nil {
		t.Fatalf("disabled platform variable was resolved: %v", err)
	}
	if _, ok := doc.Platforms["bluebubbles"]; ok {
		t.Fatal("disabled platform survived pruning")
	}
	if _, ok := doc.Platforms["discord"]; !ok {
		t.Fatal("enabled platform was pruned")
	}
}
