package startup

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

func TestPublishPluginEnvPassthrough(t *testing.T) {
	process.ClearEnvPassthrough()
	defer process.ClearEnvPassthrough()
	log := config.NewLogger(config.LevelWarn)
	m := plugins.NewManager(t.TempDir(), "default", log)
	m.Discover(map[string]plugins.Manifest{
		"demo": {Name: "demo", Key: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresEnv: []string{"SYNTH_DEMO_VAR", "OPENAI_API_KEY"}},
		"off":  {Name: "off", Key: "off", Source: "user", ManifestVersion: 2, ApiVersion: "1", RequiresEnv: []string{"SYNTH_OFF_VAR"}},
	})
	if err := m.SetEnabled("demo", true); err != nil {
		t.Fatal(err)
	}
	publishPluginEnvPassthrough(m, "default", log)
	got := process.PassthroughNames()
	if len(got) != 1 || got[0] != "SYNTH_DEMO_VAR" {
		t.Fatalf("passthrough = %v", got)
	}
	// Nil manager and empty registries are no-ops, never failures.
	publishPluginEnvPassthrough(nil, "default", log)
}
