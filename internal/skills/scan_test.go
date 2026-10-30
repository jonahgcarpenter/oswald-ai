package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSkillMD(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestScanFindsSkillsSkipsNoise(t *testing.T) {
	root := t.TempDir()
	writeSkillMD(t, filepath.Join(root, "review"), "---\nname: review\ndescription: Review code\n---\nBody\n")
	writeSkillMD(t, filepath.Join(root, "nested", "deep", "plan"), "---\nversion: 1\n---\nBody\n")
	writeSkillMD(t, filepath.Join(root, ".hidden", "ghost"), "---\nname: ghost\ndescription: hidden\n---\nBody\n")
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("nope"), 0600); err != nil {
		t.Fatal(err)
	}
	entries := scanRoots(root)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	byName := map[string]RawEntry{}
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	if byName["review"].Description != "Review code" {
		t.Fatalf("unexpected review entry: %+v", byName["review"])
	}
	// No declared name: directory name fallback with body-derived description.
	if byName["plan"].Description != "Body" {
		t.Fatalf("unexpected plan entry: %+v", byName["plan"])
	}
}

func TestScanSkipsUnclosedFence(t *testing.T) {
	root := t.TempDir()
	writeSkillMD(t, filepath.Join(root, "broken"), "---\nname: broken\nno end\n")
	if entries := scanRoots(root); len(entries) != 0 {
		t.Fatalf("expected no entries, got %+v", entries)
	}
}

func TestScanEmptyRoots(t *testing.T) {
	if entries := scanRoots(""); len(entries) != 0 {
		t.Fatalf("expected no entries, got %+v", entries)
	}
	if entries := scanRoots("/nonexistent-oswald-test-dir"); len(entries) != 0 {
		t.Fatalf("expected no entries, got %+v", entries)
	}
}
