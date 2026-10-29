package config

import (
	"strings"
	"testing"
)

// Skills need no configuration: the profile skills/ tree is always active.
// A tools.skills section is rejected as an unknown key under strict decoding.
func TestSkillsSectionRejected(t *testing.T) {
	root := browserConfigFixture(t, "  skills:\n    source_dir: \"shared/skills\"\n")
	_, err := LoadProfiles(root)
	if err == nil {
		t.Fatal("tools.skills section accepted")
	}
	if msg := err.Error(); strings.Contains(msg, "shared/skills") {
		t.Fatalf("config content leaked: %v", err)
	}
}
