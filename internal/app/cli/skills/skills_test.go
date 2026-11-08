package skills

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const skillsTestConfig = `providers:
  fake:
    api: "http://127.0.0.1:9999/v1"
model:
  provider: custom:fake
  default: fake/model
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
`

func skillsTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(skillsTestConfig), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "api"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func skillsRun(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func skillSourceFixture(t *testing.T, name, extra string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	body := "---\nname: " + name + "\ndescription: synthetic test skill\n" + extra + "---\n\nBody text.\n"
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestListEmptyAndInstall(t *testing.T) {
	root := skillsTestRoot(t)
	if code, out, _ := skillsRun(t, root, "list"); code != 0 || !strings.Contains(out, "no skills") {
		t.Fatalf("list = %d %q", code, out)
	}
	src := skillSourceFixture(t, "demo", "required_env: [SYNTH_DEMO_VAR]\n")
	if code, out, stderr := skillsRun(t, root, "install", src); code != 0 {
		t.Fatalf("install = %d %s %s", code, out, stderr)
	}
	if code, out, _ := skillsRun(t, root, "list"); code != 0 || !strings.Contains(out, "demo") {
		t.Fatalf("list = %d %q", code, out)
	}
	code, out, _ := skillsRun(t, root, "info", "demo")
	if code != 0 || !strings.Contains(out, "installed_from: local unpinned") || !strings.Contains(out, "SYNTH_DEMO_VAR") {
		t.Fatalf("info = %d %q", code, out)
	}
	if code, out, _ := skillsRun(t, root, "install", src); code != 0 || !strings.Contains(out, "already installed") {
		t.Fatalf("idempotent reinstall = %d %q", code, out)
	}
}

func TestInstallRemoveRoundTrip(t *testing.T) {
	root := skillsTestRoot(t)
	src := skillSourceFixture(t, "victim", "")
	if code, _, stderr := skillsRun(t, root, "install", src, "--profile", "alice"); code != 0 {
		t.Fatalf("install = %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "profiles", "alice", "skills", "victim", "SKILL.md")); err != nil {
		t.Fatalf("per-profile install missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "victim")); !os.IsNotExist(err) {
		t.Fatal("install leaked into default profile")
	}
	if code, _, stderr := skillsRun(t, root, "remove", "victim", "--profile", "alice"); code != 0 {
		t.Fatalf("remove = %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "profiles", "alice", "skills", "victim")); !os.IsNotExist(err) {
		t.Fatal("skill survived remove")
	}
	if code, _, _ := skillsRun(t, root, "remove", "victim", "--profile", "alice"); code == 0 {
		t.Fatal("second remove accepted")
	}
}

func TestInstallValidation(t *testing.T) {
	root := skillsTestRoot(t)
	if code, _, _ := skillsRun(t, root, "install", "notaurl with spaces"); code == 0 {
		t.Fatal("bad source accepted")
	}
	src := skillSourceFixture(t, "pinned", "")
	if code, _, _ := skillsRun(t, root, "install", src, "--ref", "main"); code == 0 {
		t.Fatal("bad ref accepted")
	}
	if code, _, _ := skillsRun(t, root, "install", src, "--ref", "0123456789abcdef0123456789abcdef01234567"); code == 0 {
		t.Fatal("local ref accepted")
	}
	if code, _, _ := skillsRun(t, root, "update", "ghost"); code == 0 {
		t.Fatal("unknown update accepted")
	}
	if code, _, _ := skillsRun(t, root, "remove", "ghost"); code == 0 {
		t.Fatal("unknown remove accepted")
	}
}

func TestScopeFencing(t *testing.T) {
	root := skillsTestRoot(t)
	bob := filepath.Join(root, "profiles", "bob")
	if err := os.MkdirAll(bob, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", bob)
	src := skillSourceFixture(t, "scoped", "")
	if code, _, _ := skillsRun(t, root, "install", src, "--profile", "default"); code == 0 {
		t.Fatal("contradicting profile install accepted")
	}
	if code, _, stderr := skillsRun(t, root, "install", src); code != 0 {
		t.Fatalf("own-scope install = %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(bob, "skills", "scoped", "SKILL.md")); err != nil {
		t.Fatalf("calling-scope install misplaced: %v", err)
	}
}
