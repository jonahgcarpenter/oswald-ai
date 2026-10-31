package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = `providers:
  fake:
    api: http://localhost:9999/v1
    key: test-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_host: 127.0.0.1
      api_port: 12345
runtime:
  log_level: info
`

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(testConfig), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func run(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestShowRedactsSecrets(t *testing.T) {
	root := testRoot(t)
	code, out, _ := run(t, root, "show")
	if code != 0 {
		t.Fatalf("show failed: %d", code)
	}
	if strings.Contains(out, "test-key") {
		t.Fatal("secret leaked in show output")
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatal("redaction marker missing")
	}
}

func TestSetGetUndoRoundTrip(t *testing.T) {
	root := testRoot(t)
	if code, _, stderr := run(t, root, "set", "runtime.log_level", "debug"); code != 0 {
		t.Fatalf("set failed: %s", stderr)
	}
	code, out, _ := run(t, root, "get", "runtime.log_level")
	if code != 0 || !strings.Contains(out, "debug") {
		t.Fatalf("get failed: %d %q", code, out)
	}
	code, out, _ = run(t, root, "diff")
	if code != 0 || strings.Contains(out, "no changes") {
		t.Fatalf("diff missing changes: %d %q", code, out)
	}
	if code, _, stderr := run(t, root, "undo"); code != 0 {
		t.Fatalf("undo failed: %s", stderr)
	}
	code, out, _ = run(t, root, "get", "runtime.log_level")
	if code != 0 || !strings.Contains(out, "info") {
		t.Fatalf("undo did not restore: %d %q", code, out)
	}
}

func TestSetInvalidLeavesFileUntouched(t *testing.T) {
	root := testRoot(t)
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	code, _, stderr := run(t, root, "set", "model.context_length", "not-a-number")
	if code == 0 {
		t.Fatal("invalid set accepted")
	}
	if stderr == "" || strings.Contains(stderr, "not-a-number") {
		t.Fatalf("bad diagnostic: %q", stderr)
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("file mutated on failed validation")
	}
}

func TestSetUnknownKeyRejected(t *testing.T) {
	root := testRoot(t)
	if code, _, _ := run(t, root, "set", "unknown_section.foo", "bar"); code == 0 {
		t.Fatal("unknown key accepted")
	}
}

func TestProfileResolvesFromOswaldHome(t *testing.T) {
	root := testRoot(t)
	bobYAML := strings.Replace(testConfig, "log_level: info", "log_level: debug", 1)
	if err := os.MkdirAll(filepath.Join(root, "profiles", "bob"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profiles", "bob", "config.yaml"), []byte(bobYAML), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", filepath.Join(root, "profiles", "bob"))
	code, out, _ := run(t, root, "get", "runtime.log_level")
	if code != 0 || !strings.Contains(out, "debug") {
		t.Fatalf("env-scoped get = %d %q", code, out)
	}
	if code, _, _ := run(t, root, "--profile", "alice", "get", "runtime.log_level"); code == 0 {
		t.Fatal("cross-profile --profile accepted under OSWALD_HOME")
	}
}

func TestReloadValidates(t *testing.T) {
	root := testRoot(t)
	if code, _, _ := run(t, root, "reload"); code != 0 {
		t.Fatal("valid reload rejected")
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("platforms: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, root, "reload"); code == 0 {
		t.Fatal("invalid reload accepted")
	}
}
