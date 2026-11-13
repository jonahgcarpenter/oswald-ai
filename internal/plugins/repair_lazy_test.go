package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// RepairProfile must be permission-fix-only: a profile that never installed
// a plugin keeps no plugins/ directory after boot repair.
func TestRepairProfileCreatesNothing(t *testing.T) {
	profile := t.TempDir()
	RepairProfile(profile)
	if _, err := os.Lstat(ProfilePluginsDir(profile)); !os.IsNotExist(err) {
		t.Fatalf("plugins exists after repair (err=%v), want absent", err)
	}
}

// RepairProfile on an existing install tree keeps it intact.
func TestRepairProfileKeepsExistingInstalls(t *testing.T) {
	profile := t.TempDir()
	writeProfileManifest(t, profile, "demo", "")
	RepairProfile(profile)
	if _, err := os.Lstat(ProfilePluginsDir(profile)); err != nil {
		t.Fatalf("plugins missing after repair: %v", err)
	}
}

// RegenerateShims with zero installs leaves no .bin residue instead of an
// empty shim directory.
func TestRegenerateShimsEmptyRemovesBin(t *testing.T) {
	profile := t.TempDir()
	if err := os.MkdirAll(ProfilePluginsDir(profile), 0700); err != nil {
		t.Fatal(err)
	}
	bin := PluginShimDir(profile)
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := RegenerateShims(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(bin); !os.IsNotExist(err) {
		t.Fatalf(".bin exists after empty regenerate (err=%v), want absent", err)
	}
}

// RegenerateShims on a profile without a plugins dir at all stays a no-op.
func TestRegenerateShimsMissingDirIsNoop(t *testing.T) {
	profile := t.TempDir()
	if err := RegenerateShims(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(profile, "plugins")); !os.IsNotExist(err) {
		t.Fatalf("plugins exists after regenerate (err=%v), want absent", err)
	}
}
