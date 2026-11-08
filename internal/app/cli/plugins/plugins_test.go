package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pluginsTestConfig = `providers:
  fake:
    api: "http://127.0.0.1:9999/v1"
model:
  provider: custom:fake
  default: fake/model
gateway:
  multiplex_profiles: true
  profile_routes:
    - platform: discord
      user_id: "123"
      profile: alice
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
`

func pluginsTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(pluginsTestConfig), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "api"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writePluginManifest(t, filepath.Join(root, "plugins-available", "demo"), "name: demo\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\n")
	return root
}

func writePluginManifest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func pluginsRun(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestListEmptyAndEnable(t *testing.T) {
	root := pluginsTestRoot(t)
	if code, out, _ := pluginsRun(t, root, "list"); code != 0 || !strings.Contains(out, "demo") {
		t.Fatalf("list = %d %q", code, out)
	}
	if code, _, stderr := pluginsRun(t, root, "enable", "demo"); code != 0 {
		t.Fatalf("enable = %d %s", code, stderr)
	}
	if code, out, _ := pluginsRun(t, root, "list"); code != 0 || !strings.Contains(out, "enabled") {
		t.Fatalf("list enabled = %d %q", code, out)
	}
	if code, _, _ := pluginsRun(t, root, "enable", "ghost"); code == 0 {
		t.Fatal("unknown enable accepted")
	}
}

func TestDisableWinsAndInfoRedacts(t *testing.T) {
	root := pluginsTestRoot(t)
	if code, _, stderr := pluginsRun(t, root, "enable", "demo"); code != 0 {
		t.Fatalf("enable = %d %s", code, stderr)
	}
	if code, _, stderr := pluginsRun(t, root, "disable", "demo"); code != 0 {
		t.Fatalf("disable = %d %s", code, stderr)
	}
	if code, out, _ := pluginsRun(t, root, "list"); code != 0 || !strings.Contains(out, "disabled") {
		t.Fatalf("list disabled = %d %q", code, out)
	}
	code, out, _ := pluginsRun(t, root, "info", "demo")
	if code != 0 {
		t.Fatalf("info = %d", code)
	}
	for _, want := range []string{"name: demo", "source: user", "state: disabled"} {
		if !strings.Contains(out, want) {
			t.Fatalf("info missing %q: %q", want, out)
		}
	}
}

func TestBinPathAndInstallDepsGates(t *testing.T) {
	root := pluginsTestRoot(t)
	if code, _, _ := pluginsRun(t, root, "bin-path", "ghost"); code == 0 {
		t.Fatal("unknown bin-path accepted")
	}
	if code, _, _ := pluginsRun(t, root, "bin-path", "demo"); code == 0 {
		t.Fatal("missing install bin-path accepted")
	}
	if code, _, _ := pluginsRun(t, root, "install-deps", "ghost"); code == 0 {
		t.Fatal("unknown install-deps accepted")
	}
	if code, _, _ := pluginsRun(t, root, "install-deps", "demo"); code == 0 {
		t.Fatal("install-deps without node deps accepted")
	}
	if code, _, _ := pluginsRun(t, root, "install-deps", "demo", "--profile", "alice"); code != 2 {
		t.Fatalf("profile-scoped install-deps = %d, want usage", code)
	}
	if code, _, _ := pluginsRun(t, root, "bin-path", "demo", "--profile", "alice"); code != 2 {
		t.Fatalf("profile-scoped bin-path = %d, want usage", code)
	}
}

func installSourceFixture(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	writePluginManifest(t, dir, "name: "+name+"\nversion: \"1.0\"\ndescription: synthetic install source\nmanifest_version: 2\napi_version: \"1\"\n")
	return dir
}

func TestInstallLocalWithEnable(t *testing.T) {
	root := pluginsTestRoot(t)
	src := installSourceFixture(t, "shipped")
	if code, out, stderr := pluginsRun(t, root, "install", src, "--enable"); code != 0 {
		t.Fatalf("install = %d %s %s", code, out, stderr)
	}
	if code, out, _ := pluginsRun(t, root, "list"); code != 0 || !strings.Contains(out, "shipped") || !strings.Contains(out, "enabled") {
		t.Fatalf("list = %d %q", code, out)
	}
	code, out, _ := pluginsRun(t, root, "info", "shipped")
	if code != 0 || !strings.Contains(out, "installed_from: local") {
		t.Fatalf("info = %d %q", code, out)
	}
	code, out, _ = pluginsRun(t, root, "install", src)
	if code != 0 || !strings.Contains(out, "already installed shipped") {
		t.Fatalf("idempotent reinstall = %d %q", code, out)
	}
	if code, _, stderr := pluginsRun(t, root, "install", src, "--force"); code != 0 {
		t.Fatalf("force reinstall = %d %s", code, stderr)
	}
}

func TestInstallScopeFencing(t *testing.T) {
	root := pluginsTestRoot(t)
	bob := filepath.Join(root, "profiles", "bob")
	if err := os.MkdirAll(bob, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", bob)
	src := installSourceFixture(t, "scoped")
	if code, _, _ := pluginsRun(t, root, "install", src); code == 0 {
		t.Fatal("named-scope install accepted")
	}
	if code, _, _ := pluginsRun(t, root, "install", src, "--profile", "default"); code == 0 {
		t.Fatal("contradicting profile install accepted")
	}
}

func TestInstallValidation(t *testing.T) {
	root := pluginsTestRoot(t)
	if code, _, _ := pluginsRun(t, root, "install", "notaurl with spaces"); code == 0 {
		t.Fatal("bad source accepted")
	}
	src := installSourceFixture(t, "pinned")
	if code, _, _ := pluginsRun(t, root, "install", src, "--ref", "main"); code == 0 {
		t.Fatal("bad ref accepted")
	}
	if code, _, _ := pluginsRun(t, root, "install", src, "--enable", "--no-enable"); code == 0 {
		t.Fatal("conflicting flags accepted")
	}
}

func TestRemoveAndUpdateGates(t *testing.T) {
	root := pluginsTestRoot(t)
	src := installSourceFixture(t, "victim")
	if code, _, stderr := pluginsRun(t, root, "install", src, "--enable"); code != 0 {
		t.Fatalf("install = %d %s", code, stderr)
	}
	if code, _, _ := pluginsRun(t, root, "update", "victim"); code == 0 {
		t.Fatal("update of unmanaged install accepted")
	}
	if code, _, _ := pluginsRun(t, root, "remove", "ghost"); code == 0 {
		t.Fatal("unknown remove accepted")
	}
	// Reference from another profile blocks removal of shared code.
	alice := filepath.Join(root, "profiles", "alice")
	if err := os.MkdirAll(alice, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alice, "config.yaml"), []byte("plugins:\n  enabled: [victim]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := pluginsRun(t, root, "remove", "victim"); code == 0 {
		t.Fatalf("referenced remove accepted: %s", stderr)
	}
	if code, _, stderr := pluginsRun(t, root, "disable", "victim", "--profile", "alice"); code != 0 {
		t.Fatalf("alice disable = %d %s", code, stderr)
	}
	storage := filepath.Join(root, "plugins", "victim")
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := pluginsRun(t, root, "remove", "victim", "--full"); code != 0 {
		t.Fatalf("remove = %d %s %s", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins-available", "victim")); !os.IsNotExist(err) {
		t.Fatal("code dir survived remove")
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Fatal("storage survived --full")
	}
}
