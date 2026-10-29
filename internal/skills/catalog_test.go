package skills

import (
	"path/filepath"
	"testing"
)

func managedEntry(root, dir, name string) RawEntry {
	return RawEntry{Name: name, Description: "d", Dir: filepath.Join(root, dir), File: filepath.Join(root, dir, "SKILL.md"), Tier: TierManaged, Root: root}
}

func TestManagedBeatsExternal(t *testing.T) {
	managed, external := t.TempDir(), t.TempDir()
	entries := []RawEntry{
		{Name: "review", Dir: filepath.Join(external, "review"), File: filepath.Join(external, "review", "SKILL.md"), Tier: TierExternal, Root: external},
		managedEntry(managed, "review", "review"),
	}
	skills := resolveCatalog(entries, "discord")
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %+v", skills)
	}
	if skills[0].Source != SourceManaged || skills[0].LoadName != "review" {
		t.Fatalf("unexpected winner: %+v", skills[0])
	}
}

func TestSameTierDuplicatesLoadByPath(t *testing.T) {
	root := t.TempDir()
	entries := []RawEntry{managedEntry(root, filepath.Join("a", "one"), "one"), managedEntry(root, filepath.Join("b", "one"), "one")}
	skills := resolveCatalog(entries, "")
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %+v", skills)
	}
	names := map[string]bool{skills[0].LoadName: true, skills[1].LoadName: true}
	if !names["a/one"] || !names["b/one"] {
		t.Fatalf("expected path load names, got %+v", skills)
	}
}

func TestPlatformGating(t *testing.T) {
	root := t.TempDir()
	entries := []RawEntry{
		{Name: "open", Dir: filepath.Join(root, "open"), Tier: TierManaged, Root: root},
		{Name: "gated", Platforms: []string{"discord"}, Dir: filepath.Join(root, "gated"), Tier: TierManaged, Root: root},
	}
	if skills := resolveCatalog(entries, "discord"); len(skills) != 2 {
		t.Fatalf("expected 2 skills on discord, got %+v", skills)
	}
	skills := resolveCatalog(entries, "imessage")
	if len(skills) != 1 || skills[0].Name != "open" {
		t.Fatalf("expected only ungated skill, got %+v", skills)
	}
}

func TestCategoryAndOrdering(t *testing.T) {
	root := t.TempDir()
	entries := []RawEntry{
		managedEntry(root, "zebra", "zebra"),
		managedEntry(root, filepath.Join("code", "review"), "review"),
		managedEntry(root, "alpha", "alpha"),
	}
	skills := resolveCatalog(entries, "")
	if len(skills) != 3 {
		t.Fatalf("expected 3 skills, got %+v", skills)
	}
	// Sorted by category then load name: code/review, general/alpha, general/zebra.
	if skills[0].LoadName != "review" || skills[0].Category != "code" {
		t.Fatalf("unexpected first skill: %+v", skills[0])
	}
	if skills[1].LoadName != "alpha" || skills[1].Category != "general" {
		t.Fatalf("unexpected second skill: %+v", skills[1])
	}
}
