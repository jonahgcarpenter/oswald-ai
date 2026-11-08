package config

import (
	"strings"
	"testing"
)

func TestTerminalPassthroughDefaults(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig)
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TerminalEnvPassthrough) != 0 {
		t.Fatalf("passthrough defaults = %+v", cfg.TerminalEnvPassthrough)
	}
}

func TestTerminalPassthroughParses(t *testing.T) {
	root := browserConfigFixture(t, "  terminal:\n    env_passthrough: [TENOR_API_KEY, SYNTH_OTHER_VAR, TENOR_API_KEY]\n")
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TerminalEnvPassthrough) != 2 || cfg.TerminalEnvPassthrough[0] != "SYNTH_OTHER_VAR" || cfg.TerminalEnvPassthrough[1] != "TENOR_API_KEY" {
		t.Fatalf("passthrough = %+v", cfg.TerminalEnvPassthrough)
	}
}

func TestTerminalPassthroughRejects(t *testing.T) {
	for _, tools := range []string{
		"  terminal:\n    env_passthrough: [has space]\n",
		"  terminal:\n    env_passthrough: [OPENAI_API_KEY=1]\n",
	} {
		root := browserConfigFixture(t, tools)
		if _, err := LoadProfiles(root); err == nil {
			t.Fatalf("invalid passthrough accepted: %q", tools)
		} else if strings.Contains(err.Error(), "TENOR") {
			t.Fatalf("configuration value leaked: %v", err)
		}
	}
}
