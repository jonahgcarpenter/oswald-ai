package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// libraryRoot locates the repo-root skills library from this test file.
func libraryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(file))), "library")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("library missing: %v", err)
	}
	return root
}

func TestLibraryCatalogMatchesSources(t *testing.T) {
	root := libraryRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "skills-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Skills []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Skills) == 0 {
		t.Fatal("catalog lists no skills")
	}
	seen := map[string]bool{}
	for _, entry := range catalog.Skills {
		if seen[entry.Name] {
			t.Fatalf("duplicate catalog entry %q", entry.Name)
		}
		seen[entry.Name] = true
		dir := filepath.Join(root, filepath.FromSlash(entry.Path))
		name, err := ValidateBundle(dir)
		if err != nil {
			t.Fatalf("catalog entry %q invalid: %v", entry.Name, err)
		}
		if name != entry.Name {
			t.Fatalf("catalog name %q != bundle %q", entry.Name, name)
		}
	}
}

func TestLibrarySeedsInstallAndList(t *testing.T) {
	root := libraryRoot(t)
	profile := t.TempDir()
	for _, name := range []string{"code-review", "runbook", "gated"} {
		result, err := InstallSkill(t.Context(), profile, filepath.Join(root, "skills", name), "", "", false)
		if err != nil {
			t.Fatalf("install %s: %v", name, err)
		}
		if result.Name != name {
			t.Fatalf("result name = %q", result.Name)
		}
	}
	entries, err := ReadLock(profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"code-review", "runbook", "gated"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("lockfile missing %q", name)
		}
	}
	store := NewStore(filepath.Join(profile, "skills"))
	var names []string
	for _, skill := range store.List("") {
		names = append(names, skill.LoadName)
	}
	joined := strings.Join(names, ",")
	for _, name := range []string{"code-review", "runbook", "gated"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("index missing %q: %s", name, joined)
		}
	}
	viewed, err := store.View("gated", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(viewed.RequiredEnv) != 1 || viewed.RequiredEnv[0] != "SYNTH_LIBRARY_TOKEN" {
		t.Fatalf("gated required_env = %+v", viewed.RequiredEnv)
	}
}
