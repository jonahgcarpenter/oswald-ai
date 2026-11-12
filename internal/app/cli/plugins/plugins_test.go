package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/plugins"
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
	writePluginManifest(t, plugins.PluginCodeDir(root, "demo"), "name: demo\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\n")
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

func TestListPresent(t *testing.T) {
	root := pluginsTestRoot(t)
	if code, out, _ := pluginsRun(t, root, "list"); code != 0 || !strings.Contains(out, "demo") {
		t.Fatalf("list = %d %q", code, out)
	}
	if code, out, _ := pluginsRun(t, root, "list"); code != 0 || !strings.Contains(out, "enabled") {
		t.Fatalf("list enabled = %d %q", code, out)
	}
	if code, _, _ := pluginsRun(t, root, "info", "ghost"); code == 0 {
		t.Fatal("unknown info accepted")
	}
}

func TestInfoRedacts(t *testing.T) {
	root := pluginsTestRoot(t)
	code, out, _ := pluginsRun(t, root, "info", "demo")
	if code != 0 {
		t.Fatalf("info = %d", code)
	}
	for _, want := range []string{"name: demo", "source: profile", "state: enabled"} {
		if !strings.Contains(out, want) {
			t.Fatalf("info missing %q: %q", want, out)
		}
	}
}

func TestInstallDepsGates(t *testing.T) {
	root := pluginsTestRoot(t)
	if code, _, _ := pluginsRun(t, root, "install-deps", "ghost"); code == 0 {
		t.Fatal("unknown install-deps accepted")
	}
	if code, _, _ := pluginsRun(t, root, "install-deps", "demo"); code == 0 {
		t.Fatal("install-deps without node deps accepted")
	}
	// Per-profile reads: unknown plugin in another profile fails, not usage.
	if code, _, _ := pluginsRun(t, root, "install-deps", "demo", "--profile", "alice"); code == 0 {
		t.Fatal("alice install-deps accepted")
	}
}

func TestRemovedExecSurface(t *testing.T) {
	root := pluginsTestRoot(t)
	// bin-path and exec are gone: the model runs bare names in terminal.
	for _, args := range [][]string{{"bin-path", "demo"}, {"exec", "demo", "--", "x"}} {
		if code, _, _ := pluginsRun(t, root, args...); code == 0 {
			t.Fatalf("%v accepted after removal", args[0])
		}
	}
}

func installSourceFixture(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	writePluginManifest(t, dir, "name: "+name+"\nversion: \"1.0\"\ndescription: synthetic install source\nmanifest_version: 2\napi_version: \"1\"\n")
	return dir
}

func TestInstallLocal(t *testing.T) {
	root := pluginsTestRoot(t)
	src := installSourceFixture(t, "shipped")
	if code, out, stderr := pluginsRun(t, root, "install", src); code != 0 {
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
	alice := filepath.Join(root, "profiles", "alice")
	t.Setenv("OSWALD_HOME", alice)
	src := installSourceFixture(t, "scoped")
	// Calling scope (alice) installs into alice.
	if code, _, stderr := pluginsRun(t, root, "install", src); code != 0 {
		t.Fatalf("alice install = %d %s", code, stderr)
	}
	// Contradicting --profile flag fails closed.
	if code, _, _ := pluginsRun(t, root, "install", src, "--profile", "default"); code == 0 {
		t.Fatal("contradicting profile install accepted")
	}
}

func TestInstallHealsNoDepsWithoutFlags(t *testing.T) {
	root := pluginsTestRoot(t)
	src := installSourceFixture(t, "healed")
	if code, _, stderr := pluginsRun(t, root, "install", src, "--node-package", "agent-data", "--node-version", "0.13.0", "--no-deps"); code != 0 {
		t.Fatalf("install = %d %s", code, stderr)
	}
	// Fake the sidecar binary so the healing path verifies without npm.
	binDir := filepath.Join(plugins.PluginCodeDir(root, "healed"), "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "agent-data"), []byte("#!/bin/sh\necho synthetic\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// No flags, no --force: the recorded spec is reused.
	if code, out, stderr := pluginsRun(t, root, "install", src); code != 0 || !strings.Contains(out, "already installed healed") {
		t.Fatalf("healing reinstall = %d %q %s", code, out, stderr)
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
	if code, _, _ := pluginsRun(t, root, "install", src, "--bogus-flag"); code == 0 {
		t.Fatal("bogus flag accepted")
	}
}

func TestRemoveAndUpdateGates(t *testing.T) {
	root := pluginsTestRoot(t)
	src := installSourceFixture(t, "victim")
	if code, _, stderr := pluginsRun(t, root, "install", src); code != 0 {
		t.Fatalf("install = %d %s", code, stderr)
	}
	if code, _, _ := pluginsRun(t, root, "update", "victim"); code == 0 {
		t.Fatal("update of local install accepted")
	}
	if code, _, _ := pluginsRun(t, root, "remove", "ghost"); code == 0 {
		t.Fatal("unknown remove accepted")
	}
	// Data survives plain remove.
	dataHome := filepath.Join(plugins.PluginDataDir(root, "victim"), "home")
	if err := os.MkdirAll(dataHome, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dataHome, "keep.txt")
	if err := os.WriteFile(marker, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := pluginsRun(t, root, "remove", "victim"); code != 0 {
		t.Fatalf("remove = %d %s", code, stderr)
	}
	if _, err := os.Stat(plugins.PluginCodeDir(root, "victim")); !os.IsNotExist(err) {
		t.Fatal("code dir survived remove")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("data did not survive plain remove")
	}
	if code, _, stderr := pluginsRun(t, root, "remove", "victim", "--full"); code != 0 {
		t.Fatalf("full remove = %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins", "victim")); !os.IsNotExist(err) {
		t.Fatal("plugin dir survived --full")
	}
}

func TestInstallNodeFlagsEndToEnd(t *testing.T) {
	root := pluginsTestRoot(t)
	src := installSourceFixture(t, "flagged")
	if code, out, stderr := pluginsRun(t, root, "install", src, "--node-package", "agent-data", "--node-version", "0.13.0", "--no-deps"); code != 0 {
		t.Fatalf("install = %d %s %s", code, out, stderr)
	}
	code, out, _ := pluginsRun(t, root, "info", "flagged")
	if code != 0 {
		t.Fatalf("info = %d %q", code, out)
	}
	if code, _, _ := pluginsRun(t, root, "install", src, "--node-package", "agent-data"); code == 0 {
		t.Fatal("half node flags accepted")
	}
	// Idempotent reinstall with the same flags stays a no-op (no npm run).
	if code, out, _ := pluginsRun(t, root, "install", src, "--node-package", "agent-data", "--node-version", "0.13.0", "--no-deps"); code != 0 || !strings.Contains(out, "already installed flagged") {
		t.Fatalf("idempotent reinstall = %d %q", code, out)
	}
	if code, _, _ := pluginsRun(t, root, "install", src, "--node-package", "other", "--node-version", "9.9.9", "--no-deps"); code == 0 {
		t.Fatal("node mismatch accepted without --force")
	}
}
