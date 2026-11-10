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

func bindPlugin(t *testing.T, store *Store, key string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), key, "skills", "shipped")
	writeSkillMD(t, dir, "---\nname: shipped\ndescription: Synthetic bundled skill\n---\n\nBundled body.\n")
	store.BindPlugin(key, filepath.Join(t.TempDir(), "unused"))
	store.BindPlugin(key, filepath.Dir(dir))
	return dir
}

func TestBindPluginNamespacedAndReadOnly(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	bindPlugin(t, store, "acme")
	found := store.List("")
	if len(found) != 3 {
		t.Fatalf("expected 3 skills, got %+v", found)
	}
	var bundled Skill
	for _, skill := range found {
		if skill.Source == "acme" {
			bundled = skill
		}
	}
	if bundled.LoadName != "acme/shipped" || bundled.Category != "plugin acme" {
		t.Fatalf("bad namespacing: %+v", bundled)
	}
	viewed, err := store.View("acme/shipped", "", "")
	if err != nil || !strings.Contains(viewed.Body, "Bundled body.") {
		t.Fatalf("view = %+v %v", viewed, err)
	}
	if _, err := store.Update("acme/shipped", "Bundled", "Changed"); err == nil {
		t.Fatal("plugin skill update accepted")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("wrong read-only error: %v", err)
	}
	if index := store.Index(""); !strings.Contains(index, "acme/shipped") {
		t.Fatalf("index missing plugin skill:\n%s", index)
	}
}

func TestBindPluginCollisionPrefersProfile(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	bindPlugin(t, store, "acme")
	other := filepath.Join(t.TempDir(), "other", "skills", "review")
	writeSkillMD(t, other, "---\nname: review\ndescription: Bundled review\n---\n\nOther body.\n")
	store.BindPlugin("other", filepath.Dir(other))
	// The bare name resolves to the profile skill; the namespaced name
	// reaches the plugin copy. Namespacing disambiguates by construction.
	viewed, err := store.View("review", "", "")
	if err != nil || !strings.Contains(viewed.Body, "Follow the checklist.") {
		t.Fatalf("view = %+v %v", viewed, err)
	}
	viewed, err = store.View("other/review", "", "")
	if err != nil || !strings.Contains(viewed.Body, "Other body.") {
		t.Fatalf("view = %+v %v", viewed, err)
	}
	if _, err := store.View("acme/review", "", ""); err == nil {
		t.Fatal("cross-plugin name accepted")
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

func TestBindPluginJobSearchShape(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	// Job-search layout: skills/<name>/SKILL.md plus scripts and required_env.
	root := filepath.Join(t.TempDir(), "job-search", "skills")
	writeSkillMD(t, filepath.Join(root, "job-search"),
		"---\nname: job-search\ndescription: Find matching jobs\nrequired_env: [SYNTH_AGENT_DATA_KEY]\n---\n\nSearch body.\n")
	writeSkillMD(t, filepath.Join(root, "job-search-run"),
		"---\nname: job-search-run\ndescription: Run the search loop\n---\n\nRun body.\n")
	scripts := filepath.Join(root, "job-search-run", "scripts")
	if err := os.MkdirAll(scripts, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "search-jobs.sh"), []byte("#!/bin/sh\necho search\n"), 0755); err != nil {
		t.Fatal(err)
	}
	store.BindPlugin("job-search", root)
	found := store.List("")
	if len(found) != 4 {
		t.Fatalf("expected 4 skills, got %+v", found)
	}
	byLoad := map[string]Skill{}
	for _, skill := range found {
		byLoad[skill.LoadName] = skill
	}
	for _, want := range []string{"job-search/job-search", "job-search/job-search-run"} {
		skill, ok := byLoad[want]
		if !ok {
			t.Fatalf("missing bundled skill %q in %+v", want, found)
		}
		if skill.Source != "job-search" || skill.Category != "plugin job-search" {
			t.Fatalf("bad namespacing: %+v", skill)
		}
	}
	viewed, err := store.View("job-search/job-search-run", "", "")
	if err != nil || !strings.Contains(viewed.Body, "Run body.") {
		t.Fatalf("view = %+v %v", viewed, err)
	}
	viewed, err = store.View("job-search/job-search", "", "")
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if len(viewed.RequiredEnv) != 1 || viewed.RequiredEnv[0] != "SYNTH_AGENT_DATA_KEY" {
		t.Fatalf("required_env = %+v", viewed.RequiredEnv)
	}
	if _, err := store.View("job-search/job-search-run", "scripts/search-jobs.sh", ""); err != nil {
		t.Fatalf("supporting file: %v", err)
	}
}

func TestCreateUpdateRemoveSupportingFile(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	skill, err := store.CreateFile("review", "references/api.md", "# API\nUse v2.\n")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if skill.LoadName != "review" {
		t.Fatalf("load name = %q", skill.LoadName)
	}
	info, err := os.Stat(filepath.Join(managed, "review", "references", "api.md"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("bad perms: %+v %v", info, err)
	}
	viewed, err := store.View("review", "references/api.md", "")
	if err != nil || !strings.Contains(viewed.FileContent, "Use v2.") {
		t.Fatalf("view file = %+v %v", viewed, err)
	}
	if _, err := store.CreateFile("review", "references/api.md", "dup"); err == nil {
		t.Fatal("duplicate create accepted")
	}
	if _, err := store.UpdateFile("review", "references/api.md", "Use v2.", "Use v3."); err != nil {
		t.Fatalf("update file: %v", err)
	}
	viewed, err = store.View("review", "references/api.md", "")
	if err != nil || !strings.Contains(viewed.FileContent, "Use v3.") {
		t.Fatalf("updated view = %+v %v", viewed, err)
	}
	if _, err := store.UpdateFile("review", "references/api.md", " ", "_"); err == nil {
		t.Fatal("ambiguous old_text accepted")
	}
	if _, err := store.RemoveFile("review", "references/api.md"); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if _, err := store.View("review", "references/api.md", ""); err == nil {
		t.Fatal("removed file still readable")
	}
	if _, err := store.RemoveFile("review", "SKILL.md"); err == nil {
		t.Fatal("SKILL.md remove accepted")
	}
}

func TestSupportFileWritesRefused(t *testing.T) {
	managed := seedLibrary(t)
	store := NewStore(managed)
	for _, tc := range []struct {
		name string
		file string
		op   string
	}{
		{"review", "../escape.md", "create"},
		{"review", "/abs.md", "create"},
		{"review", "SKILL.md", "create"},
		{"review", "notes.md", "create"},
		{"review", "references/../SKILL.md", "create"},
		{"review", ".hidden.md", "create"},
		{"review", "references/../../escape.md", "update"},
		{"ghost", "references/a.md", "create"},
	} {
		var err error
		switch tc.op {
		case "create":
			_, err = store.CreateFile(tc.name, tc.file, "content")
		case "update":
			_, err = store.UpdateFile(tc.name, tc.file, "x", "y")
		}
		if err == nil {
			t.Fatalf("%s %s accepted", tc.op, tc.file)
		}
	}
	bindPlugin(t, store, "acme")
	for _, fn := range []func() error{
		func() error { _, err := store.CreateFile("acme/shipped", "references/a.md", "x"); return err },
		func() error { _, err := store.UpdateFile("acme/shipped", "references/a.md", "x", "y"); return err },
		func() error { _, err := store.RemoveFile("acme/shipped", "references/a.md"); return err },
	} {
		if err := fn(); err == nil {
			t.Fatal("plugin skill file write accepted")
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("wrong read-only error: %v", err)
		}
	}
	if _, err := store.RemoveFile("review", "references/missing.md"); err == nil {
		t.Fatal("missing remove accepted")
	}
}
