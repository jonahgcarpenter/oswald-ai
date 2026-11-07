package plugins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func TestParseInstallSourceShapes(t *testing.T) {
	git, err := ParseInstallSource("acme/widgets")
	if err != nil || git.Kind != "git" || git.URL != "https://github.com/acme/widgets.git" {
		t.Fatalf("shorthand = %+v %v", git, err)
	}
	https, err := ParseInstallSource("https://example.com/org/repo.git")
	if err != nil || https.Kind != "git" {
		t.Fatalf("https = %+v %v", https, err)
	}
	if _, err := ParseInstallSource("http://example.com/org/repo.git"); err == nil {
		t.Fatal("http accepted")
	}
	if _, err := ParseInstallSource("git@example.com:org/repo.git"); err == nil {
		t.Fatal("ssh accepted")
	}
	dir := t.TempDir()
	file, err := ParseInstallSource("file://" + dir)
	if err != nil || file.Kind != "local" || file.URL != dir {
		t.Fatalf("file = %+v %v", file, err)
	}
	for _, bad := range []string{"", "has space/repo", "a/b/c", "../escape", "https://example.com/a b.git"} {
		if _, err := ParseInstallSource(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := ParseInstallSource(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing dir accepted")
	}
}

func TestValidateInstallRefAndSubdir(t *testing.T) {
	if err := ValidateInstallRef(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInstallRef("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"main", "v1.0", "01234567", "0123456789abcdef0123456789abcdef0123456g"} {
		if err := ValidateInstallRef(bad); err == nil {
			t.Fatalf("accepted ref %q", bad)
		}
	}
	if _, err := ValidateInstallSubdir(""); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateInstallSubdir("../escape"); err == nil {
		t.Fatal("escape accepted")
	}
	if _, err := ValidateInstallSubdir("/abs"); err == nil {
		t.Fatal("absolute accepted")
	}
}

func installFixture(t *testing.T, dir, name, extra string) {
	t.Helper()
	body := "name: " + name + "\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\n" + extra
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFromLocalDir(t *testing.T) {
	global := t.TempDir()
	src := filepath.Join(t.TempDir(), "src-plugin")
	installFixture(t, src, "local-widget", "")
	log := config.NewLogger(config.LevelWarn)
	result, err := InstallFromURL(t.Context(), global, src, "", "", false, log)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Key != "local-widget" || result.Pinned {
		t.Fatalf("result = %+v", result)
	}
	dest := filepath.Join(global, "plugins-available", "local-widget")
	if _, err := os.Stat(filepath.Join(dest, "plugin.yaml")); err != nil {
		t.Fatalf("published files missing: %v", err)
	}
	meta, err := ReadInstallMetadata(global, "local-widget")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.Source != "local" || meta.URL != src || meta.Pinned {
		t.Fatalf("meta = %+v", meta)
	}
	if _, err := InstallFromURL(t.Context(), global, src, "", "", false, log); err == nil {
		t.Fatal("duplicate install accepted")
	}
	if _, err := InstallFromURL(t.Context(), global, src, "", "", true, log); err != nil {
		t.Fatalf("force reinstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(global, "plugins-backup")); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func TestInstallRejectsBadManifest(t *testing.T) {
	global := t.TempDir()
	src := filepath.Join(t.TempDir(), "bad-plugin")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "plugin.yaml"), []byte("name: bad\n"), 0644); err != nil {
		t.Fatal(err)
	}
	log := config.NewLogger(config.LevelWarn)
	if _, err := InstallFromURL(t.Context(), global, src, "", "", false, log); err == nil {
		t.Fatal("bad manifest accepted")
	}
	if _, err := InstallFromURL(t.Context(), global, src, "main", "", false, log); err == nil {
		t.Fatal("bad ref accepted")
	}
}

func TestRemoveInstallUnknown(t *testing.T) {
	if err := RemoveInstall(t.TempDir(), "ghost"); err == nil {
		t.Fatal("unknown remove accepted")
	}
	if err := RemoveInstall(t.TempDir(), "nope!"); err == nil {
		t.Fatal("bad key accepted")
	}
}
