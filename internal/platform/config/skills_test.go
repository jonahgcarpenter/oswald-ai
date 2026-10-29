package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillsConfigDefaults(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig)
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkillsSourceDir != "" {
		t.Fatalf("skills source default = %q", cfg.SkillsSourceDir)
	}
}

func TestSkillsConfigResolvesRelativeToProfileRoot(t *testing.T) {
	root := browserConfigFixture(t, "  skills:\n    source_dir: \"shared/skills\"\n")
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join(root, "shared", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkillsSourceDir != want {
		t.Fatalf("skills source = %q, want %q", cfg.SkillsSourceDir, want)
	}
}

func TestSkillsConfigRejectsInvalid(t *testing.T) {
	for _, tools := range []string{
		"  skills:\n    source_dir: \"bad\x00path\"\n",
		"  skills:\n    unknown_key: \"x\"\n",
	} {
		root := browserConfigFixture(t, tools)
		_, err := LoadProfiles(root)
		if err == nil {
			t.Fatalf("invalid skills config accepted: %q", tools)
		}
		if msg := err.Error(); strings.Contains(msg, "bad") {
			t.Fatalf("config content leaked: %v", err)
		}
	}
}
