package plugins

import (
	"os"
	"path/filepath"
	"strings"
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
	again, err := InstallFromURL(t.Context(), global, src, "", "", false, log)
	if err != nil {
		t.Fatalf("idempotent reinstall: %v", err)
	}
	if !again.AlreadyInstalled || again.Key != "local-widget" {
		t.Fatalf("result = %+v", again)
	}
	other := filepath.Join(t.TempDir(), "other-plugin")
	installFixture(t, other, "local-widget", "")
	if _, err := InstallFromURL(t.Context(), global, other, "", "", false, log); err == nil {
		t.Fatal("conflicting source accepted")
	} else if !strings.Contains(err.Error(), "different source") {
		t.Fatalf("wrong conflict: %v", err)
	}
	if _, err := InstallFromURL(t.Context(), global, src, "0123456789abcdef0123456789abcdef01234567", "", false, log); err == nil {
		t.Fatal("local ref accepted")
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

func TestExistingInstallState(t *testing.T) {
	global := t.TempDir()
	if err := os.MkdirAll(filepath.Join(global, "plugins-available", "widgets"), 0755); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/acme/widgets.git"
	rev := "0123456789abcdef0123456789abcdef01234567"
	other := "fedcba9876543210fedcba9876543210fedcba98"
	if err := WriteInstallMetadata(filepath.Join(global, "plugins-available", "widgets"),
		InstallMetadata{Source: "git", URL: url, Ref: rev, Revision: rev, Pinned: true}); err != nil {
		t.Fatal(err)
	}
	src := InstallSource{Kind: "git", URL: url}
	if result, done, err := existingInstallState(global, "widgets", src, "", ""); err != nil || !done || !result.AlreadyInstalled {
		t.Fatalf("same source = %+v %v %v", result, done, err)
	}
	if _, done, err := existingInstallState(global, "widgets", src, "", rev); err != nil || !done {
		t.Fatalf("same ref = %v %v", done, err)
	}
	if _, _, err := existingInstallState(global, "widgets", src, "", other); err == nil {
		t.Fatal("moved pin accepted")
	} else if !strings.Contains(err.Error(), "update") {
		t.Fatalf("wrong moved-pin error: %v", err)
	}
	otherSrc := InstallSource{Kind: "git", URL: "https://github.com/other/widgets.git"}
	if _, _, err := existingInstallState(global, "widgets", otherSrc, "", ""); err == nil {
		t.Fatal("different source accepted")
	} else if !strings.Contains(err.Error(), "different source") {
		t.Fatalf("wrong source error: %v", err)
	}
	if _, _, err := existingInstallState(global, "widgets", src, "sub", ""); err == nil {
		t.Fatal("different subdir accepted")
	}
	// Hand-placed tree without metadata refuses without --force.
	plain := filepath.Join(global, "plugins-available", "plain")
	if err := os.MkdirAll(plain, 0755); err != nil {
		t.Fatal(err)
	}
	plainSrc := filepath.Join(t.TempDir(), "plain")
	installFixture(t, plainSrc, "plain", "")
	if _, err := InstallFromURL(t.Context(), global, plainSrc, "", "", false, config.NewLogger(config.LevelWarn)); err == nil {
		t.Fatal("unmanaged overwrite accepted")
	} else if !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("wrong unmanaged error: %v", err)
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
