package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// libraryRoot locates the repo-root plugin library from this test file.
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
	raw, err := os.ReadFile(filepath.Join(root, "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Plugins []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Plugins) == 0 {
		t.Fatal("catalog lists no plugins")
	}
	seen := map[string]bool{}
	for _, entry := range catalog.Plugins {
		if seen[entry.Name] {
			t.Fatalf("duplicate catalog entry %q", entry.Name)
		}
		seen[entry.Name] = true
		dir := filepath.Join(root, filepath.FromSlash(entry.Path))
		manifest, err := ParseManifestFile(dir, "profile")
		if err != nil {
			t.Fatalf("catalog entry %q invalid: %v", entry.Name, err)
		}
		if manifest.Name != entry.Name {
			t.Fatalf("catalog name %q != manifest %q", entry.Name, manifest.Name)
		}
	}
}

func TestLibrarySeedsInstall(t *testing.T) {
	root := libraryRoot(t)
	profile := t.TempDir()
	log := config.NewLogger(config.LevelWarn)
	for _, name := range []string{"hello-skills", "cli-helper", "env-gated"} {
		src := filepath.Join(root, "plugins", name)
		result, err := InstallFromURL(t.Context(), profile, src, InstallOptions{NoDeps: true}, log)
		if err != nil {
			t.Fatalf("install %s: %v", name, err)
		}
		if result.Key != name {
			t.Fatalf("key = %q", result.Key)
		}
	}
	winners := ScanProfileWinners(profile)
	for _, name := range []string{"hello-skills", "cli-helper", "env-gated"} {
		if _, ok := winners[name]; !ok {
			t.Fatalf("scan missing %q", name)
		}
	}
	// cli-helper records its sidecar pin without installing (NoDeps).
	meta, err := ReadInstallMetadata(profile, "cli-helper")
	if err != nil {
		t.Fatal(err)
	}
	if meta.NodePackage != "prettier" || meta.NodeVersion != "3.1.0" {
		t.Fatalf("sidecar pin not recorded: %+v", meta)
	}
	// env-gated stays unloaded until its variable is set.
	m := NewManager(profile, "default", log)
	m.LoadFromProfile(profile)
	for _, got := range m.EnabledNames() {
		if got == "env-gated" {
			t.Fatal("env-gated loaded without its variable")
		}
	}
	t.Setenv("SYNTH_LIBRARY_TOKEN", "synthetic")
	m2 := NewManager(profile, "default", log)
	m2.LoadFromProfile(profile)
	found := false
	for _, got := range m2.EnabledNames() {
		if got == "env-gated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("env-gated not loaded with variable set: %v", m2.EnabledNames())
	}
}
