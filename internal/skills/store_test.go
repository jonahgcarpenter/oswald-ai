package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedLibrary(t *testing.T) string {
	t.Helper()
	managed := t.TempDir()
	writeSkillMD(t, filepath.Join(managed, "review"),
		"---\nname: review\ndescription: Review code\n---\n\nFollow the checklist.\n")
	writeSkillMD(t, filepath.Join(managed, "deploy"),
		"---\nname: deploy\ndescription: Deploy things\n---\n\nShip it.\n")
	if err := os.MkdirAll(filepath.Join(managed, "review", "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "review", "scripts", "check.sh"), []byte("#!/bin/sh\necho ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return managed
}

func TestStoreListAndIndex(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	skills := store.List("")
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %+v", skills)
	}
	if index := store.Index(""); !strings.Contains(index, "- review: Review code") || !strings.Contains(index, "- deploy: Deploy things") {
		t.Fatalf("unexpected index:\n%s", index)
	}
	if got := NewStore("").Index(""); got != "" {
		t.Fatalf("expected empty index, got %q", got)
	}
}

func TestViewLoadsBodyAndSupportingFiles(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	view, err := store.View("review", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Body, "Follow the checklist.") {
		t.Fatalf("unexpected body %q", view.Body)
	}
	if len(view.Supporting) != 1 || view.Supporting[0] != "scripts/check.sh" {
		t.Fatalf("unexpected supporting files %v", view.Supporting)
	}
	file, err := store.View("review", "scripts/check.sh", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(file.FileContent, "echo ok") {
		t.Fatalf("unexpected file content %q", file.FileContent)
	}
}

func TestViewCaseInsensitiveAndDirName(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	if _, err := store.View("REVIEW", "", ""); err != nil {
		t.Fatalf("expected case-insensitive match: %v", err)
	}
	if _, err := store.View("unknown-skill", "", ""); err == nil {
		t.Fatal("expected unknown skill error")
	}
}

func TestViewAmbiguousNamesError(t *testing.T) {
	managed := t.TempDir()
	writeSkillMD(t, filepath.Join(managed, "a", "dup"), "---\nname: dup\ndescription: one\n---\nA\n")
	writeSkillMD(t, filepath.Join(managed, "b", "dup"), "---\nname: dup\ndescription: two\n---\nB\n")
	store := NewStore(managed)
	if _, err := store.View("dup", "", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous error, got %v", err)
	}
	if _, err := store.View("a/dup", "", ""); err != nil {
		t.Fatalf("expected path disambiguation to work: %v", err)
	}
}

func TestViewFilePathConfinement(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	for _, evil := range []string{"../SKILL.md", "/etc/hostname", "..", "scripts/../../SKILL.md"} {
		if _, err := store.View("review", evil, ""); err == nil {
			t.Fatalf("expected confinement error for %q", evil)
		}
	}
}

func TestCreateAndUpdate(t *testing.T) {
	store := NewStore(t.TempDir())
	skill, err := store.Create("Fresh-Skill", "Does fresh things", "Step one.\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if skill.LoadName != "fresh-skill" {
		t.Fatalf("unexpected created skill: %+v", skill)
	}
	if skill.Category != "general" {
		t.Fatalf("top-level skill category = %q, want general", skill.Category)
	}
	if _, err := store.Create("fresh-skill", "Again", "Body\n", ""); err == nil {
		t.Fatal("expected duplicate create to fail")
	}
	if _, err := store.Create("Bad Name!", "desc", "body", ""); err == nil {
		t.Fatal("expected invalid slug to fail")
	}
	for _, evil := range []string{"../escape", "a/b", "/abs", ".", ".."} {
		if _, err := store.Create("evil-skill", "desc", "body", evil); err == nil {
			t.Fatalf("expected invalid category %q to fail", evil)
		}
	}
	updated, err := store.Update("fresh-skill", "Step one.", "Step one, then two.")
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.View(updated.LoadName, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view.Body, "then two") {
		t.Fatalf("unexpected updated body %q", view.Body)
	}
	if _, err := store.Update("fresh-skill", "missing needle", "x"); err == nil {
		t.Fatal("expected non-matching old_text to fail")
	}
}

func TestCreateWithCategory(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	skill, err := store.Create("Ship", "Ship fast", "Ship it.\n", "Code")
	if err != nil {
		t.Fatal(err)
	}
	if skill.Category != "code" {
		t.Fatalf("category = %q, want code", skill.Category)
	}
	if _, err := os.Stat(filepath.Join(root, "code", "ship", "SKILL.md")); err != nil {
		t.Fatalf("categorized skill file missing: %v", err)
	}
	index := store.Index("")
	if !strings.Contains(index, "code:") || !strings.Contains(index, "- ship: Ship fast") {
		t.Fatalf("index missing categorized row:\n%s", index)
	}
	viewed, err := store.View("ship", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if viewed.Category != "code" || !strings.Contains(viewed.Body, "Ship it.") {
		t.Fatalf("unexpected categorized view: %+v", viewed)
	}
}

func TestUpdateBumpsPatchVersion(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create("bump", "Bump version", "Step one.", ""); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update("bump", "Step one.", "Step one, then two.")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != "1.0.1" {
		t.Fatalf("expected patch bump to 1.0.1, got %q", updated.Version)
	}
	// An explicit version edit is respected, not double-bumped.
	again, err := store.Update("bump", "version: 1.0.1", "version: 2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != "2.0.0" {
		t.Fatalf("expected explicit version to stick, got %q", again.Version)
	}
}

func TestUpdateRejectsLongDescription(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Create("desc", "Short desc", "Body.", ""); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 61)
	if _, err := store.Update("desc", "Short desc", long); err == nil {
		t.Fatal("expected long description to fail")
	}
}
