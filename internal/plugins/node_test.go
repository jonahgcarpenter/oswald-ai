package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateNodeDepsPins(t *testing.T) {
	valid := &Manifest{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "prettier", NodeVersion: "3.1.0"}
	if err := ValidateManifest(valid); err != nil {
		t.Fatalf("valid node deps rejected: %v", err)
	}
	if got := DefaultNodeBin(*valid); got != "prettier" {
		t.Fatalf("bin = %q", got)
	}
	scoped := &Manifest{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "@scope/tool", NodeVersion: "1.2.3", NodeBin: "tool-bin"}
	if err := ValidateManifest(scoped); err != nil {
		t.Fatalf("scoped node deps rejected: %v", err)
	}
	for _, tc := range []Manifest{
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "prettier"},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodeVersion: "1.2.3"},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "prettier", NodeVersion: "^3.1.0"},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "prettier", NodeVersion: "latest"},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "PRETTIER", NodeVersion: "3.1.0"},
		{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "prettier", NodeVersion: "3.1.0", NodeBin: "../escape"},
	} {
		candidate := tc
		if err := ValidateManifest(&candidate); err == nil {
			t.Fatalf("expected reject for %+v", tc)
		}
	}
}

func TestNodeBinPathConfinement(t *testing.T) {
	profile := t.TempDir()
	binDir := filepath.Join(PluginCodeDir(profile, "demo"), "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(PluginCodeDir(profile, "demo"), "node_modules", "demo", "bin.js")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("#!/usr/bin/env node\n"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "demo")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := NodeBinPath(profile, "demo", "demo")
	if err != nil {
		t.Fatalf("bin path: %v", err)
	}
	if got != target {
		t.Fatalf("got %q want %q", got, target)
	}
	if _, err := NodeBinPath(profile, "demo", "../escape"); err == nil {
		t.Fatal("expected escape reject")
	}
	if _, err := NodeBinPath(profile, "demo", "missing"); err == nil {
		t.Fatal("expected missing reject")
	}
}

func TestInstallNodeDepsRejectsMissing(t *testing.T) {
	manifest := Manifest{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1"}
	if err := InstallNodeDeps(t.Context(), t.TempDir(), manifest, nil); err == nil {
		t.Fatal("missing node deps accepted")
	}
}
