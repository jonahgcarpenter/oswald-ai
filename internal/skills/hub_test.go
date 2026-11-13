package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func hubFixture(t *testing.T, dir, name, extra string) {
	t.Helper()
	body := "---\nname: " + name + "\ndescription: synthetic test skill\n" + extra + "---\n\nBody text.\n"
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestParseInstallSource(t *testing.T) {
	git, err := ParseInstallSource("acme/widgets")
	if err != nil || git.Kind != "git" {
		t.Fatalf("shorthand = %+v %v", git, err)
	}
	if _, err := ParseInstallSource("https://example.com/org/repo.git"); err != nil {
		t.Fatalf("https: %v", err)
	}
	if _, err := ParseInstallSource("http://example.com/org/repo.git"); err == nil {
		t.Fatal("http accepted")
	}
	dir := t.TempDir()
	if _, err := ParseInstallSource("file://" + dir); err != nil {
		t.Fatalf("file: %v", err)
	}
	for _, bad := range []string{"", "a/b/c", "../escape", "has space"} {
		if _, err := ParseInstallSource(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestValidateInstallRefSubdir(t *testing.T) {
	if err := ValidateInstallRef(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInstallRef("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInstallRef("main"); err == nil {
		t.Fatal("tag accepted")
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

func TestValidateBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	hubFixture(t, dir, "review", "required_env: [SYNTH_A_VAR]\n")
	name, err := ValidateBundle(dir)
	if err != nil || name != "review" {
		t.Fatalf("got %q %v", name, err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.MkdirAll(bad, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBundle(bad); err == nil {
		t.Fatal("missing SKILL.md accepted")
	}
	unclosed := filepath.Join(t.TempDir(), "unclosed")
	if err := os.MkdirAll(unclosed, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unclosed, "SKILL.md"), []byte("---\nname: broken\nbody"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBundle(unclosed); err == nil {
		t.Fatal("unclosed fence accepted")
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadInstallMetadata(root, "demo"); !os.IsNotExist(err) {
		t.Fatalf("empty metadata = %v", err)
	}
	dest := filepath.Join(root, "skills", "demo")
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	want := InstallMetadata{Source: "local", URL: "/tmp/synth-demo", InstalledAt: "2026-01-01T00:00:00Z"}
	if err := WriteInstallMetadata(dest, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInstallMetadata(root, "demo")
	if err != nil || got.URL != "/tmp/synth-demo" {
		t.Fatalf("got %+v %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dest, ".install-metadata.json"))
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("metadata perms = %+v %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".install-metadata.json"), []byte("{nope"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstallMetadata(root, "demo"); err == nil {
		t.Fatal("corrupt metadata accepted")
	}
}

func TestInstallSkillLocal(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "src-skill")
	hubFixture(t, src, "shipped", "")
	result, err := InstallSkill(t.Context(), root, src, "", "", false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Name != "shipped" || result.AlreadyInstalled {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "shipped", "SKILL.md")); err != nil {
		t.Fatalf("published files missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "shipped", ".install-metadata.json")); err != nil {
		t.Fatalf("metadata missing: %v", err)
	}
	if got := scanRoots(filepath.Join(root, "skills")); len(got) != 1 || got[0].Name != "shipped" {
		t.Fatalf("scan with metadata = %+v", got)
	}
	again, err := InstallSkill(t.Context(), root, src, "", "", false)
	if err != nil || !again.AlreadyInstalled {
		t.Fatalf("idempotent = %+v %v", again, err)
	}
	other := filepath.Join(t.TempDir(), "other-src")
	hubFixture(t, other, "shipped", "")
	if _, err := InstallSkill(t.Context(), root, other, "", "", false); err == nil {
		t.Fatal("conflicting source accepted")
	} else if !strings.Contains(err.Error(), "different source") {
		t.Fatalf("wrong conflict: %v", err)
	}
	if _, err := InstallSkill(t.Context(), root, other, "", "", true); err != nil {
		t.Fatalf("force reinstall: %v", err)
	}
	if _, err := InstallSkill(t.Context(), root, src, "0123456789abcdef0123456789abcdef01234567", "", false); err == nil {
		t.Fatal("local ref accepted")
	}
}

func TestInstallSkillRejects(t *testing.T) {
	root := t.TempDir()
	if _, err := InstallSkill(t.Context(), root, "notaurl with spaces", "", "", false); err == nil {
		t.Fatal("bad source accepted")
	}
	empty := t.TempDir()
	if _, err := InstallSkill(t.Context(), root, empty, "", "", false); err == nil {
		t.Fatal("empty bundle accepted")
	}
}

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func gitRepo(t *testing.T, name, extra string) (repo, sha string) {
	t.Helper()
	if !gitAvailable() {
		t.Skip("git unavailable")
	}
	repo = filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	hubFixture(t, repo, "gitskill", extra)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=synth", "GIT_AUTHOR_EMAIL=synth@example.com", "GIT_COMMITTER_NAME=synth", "GIT_COMMITTER_EMAIL=synth@example.com")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q", "-b", "main")
	run("add", "SKILL.md")
	run("commit", "-qm", "synthetic")
	return repo, run("rev-parse", "HEAD")
}

func TestUpdateSkillLocalGit(t *testing.T) {
	repo, first := gitRepo(t, "repo", "")
	root := t.TempDir()
	result, err := InstallSkill(t.Context(), root, repo, "", "", false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Revision != first {
		t.Fatalf("revision = %+v want %s", result, first)
	}
	// Second run without changes resolves the new HEAD.
	secondFile := filepath.Join(repo, "SKILL.md")
	body, err := os.ReadFile(secondFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondFile, append(body, "\nMore.\n"...), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "SKILL.md")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	commit := exec.Command("git", "commit", "-qm", "synthetic two")
	commit.Dir = repo
	commit.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=synth", "GIT_AUTHOR_EMAIL=synth@example.com", "GIT_COMMITTER_NAME=synth", "GIT_COMMITTER_EMAIL=synth@example.com")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}
	revCmd := exec.Command("git", "rev-parse", "HEAD")
	revCmd.Dir = repo
	secondBytes, err := revCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	second := strings.TrimSpace(string(secondBytes))
	updated, err := UpdateSkill(t.Context(), root, "gitskill", "", false)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != second || updated.AlreadyInstalled {
		t.Fatalf("updated = %+v want %s", updated, second)
	}
	// Pinned without explicit --ref refuses.
	if _, err := UpdateSkill(t.Context(), root, "gitskill", second, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := UpdateSkill(t.Context(), root, "gitskill", "", false); err == nil {
		t.Fatal("pinned update without ref accepted")
	} else if !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("wrong pinned error: %v", err)
	}
	// Edited SKILL.md is overwritten like plugins: no drift guard.
	installed := filepath.Join(root, "skills", "gitskill", "SKILL.md")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, append(data, "operator edit\n"...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateSkill(t.Context(), root, "gitskill", second, true); err != nil {
		t.Fatalf("force update: %v", err)
	}
}

func TestUpdateSkillGates(t *testing.T) {
	root := t.TempDir()
	if _, err := UpdateSkill(t.Context(), root, "ghost", "", false); err == nil {
		t.Fatal("unknown update accepted")
	}
	if _, err := UpdateSkill(t.Context(), root, "Bad Name!", "", false); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestUpdateSkillOverwritesSupportingFileEdit(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git unavailable")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "references"), 0755); err != nil {
		t.Fatal(err)
	}
	hubFixture(t, repo, "gitskill", "")
	if err := os.WriteFile(filepath.Join(repo, "references", "api.md"), []byte("# API\nv1.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=synth", "GIT_AUTHOR_EMAIL=synth@example.com", "GIT_COMMITTER_NAME=synth", "GIT_COMMITTER_EMAIL=synth@example.com")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q", "-b", "main")
	run("add", "-A")
	run("commit", "-qm", "synthetic")
	root := t.TempDir()
	if _, err := InstallSkill(t.Context(), root, repo, "", "", false); err != nil {
		t.Fatalf("install: %v", err)
	}
	installed := filepath.Join(root, "skills", "gitskill", "references", "api.md")
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, append(data, "operator edit\n"...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateSkill(t.Context(), root, "gitskill", "", false); err != nil {
		t.Fatalf("update over edit: %v", err)
	}
	refreshed, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(refreshed), "operator edit") {
		t.Fatal("update kept the operator edit")
	}
}
