package providers

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
    api: http://127.0.0.1:9999/v1
    key: test-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
runtime:
  log_level: info
`

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
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

func TestListRedactsSecrets(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "list")
	if code != 0 {
		t.Fatalf("list = %d", code)
	}
	if strings.Contains(out, "test-key") {
		t.Fatal("secret leaked in list output")
	}
	if !strings.Contains(out, "fake") {
		t.Fatalf("provider missing in %q", out)
	}
}

func TestAddRemoveRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "add", "extra", "--api", "http://127.0.0.1:9998/v1")
	if code != 0 {
		t.Fatalf("add = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "provider extra added") {
		t.Fatalf("add missing confirmation = %q", out)
	}
	code, out, _ = run(t, root, "show", "extra")
	if code != 0 || !strings.Contains(out, "9998") {
		t.Fatalf("show = %d %q", code, out)
	}
	code, _, stderr = run(t, root, "remove", "extra")
	if code != 0 {
		t.Fatalf("remove = %d stderr=%s", code, stderr)
	}
}

func TestAddRejectsInvalidURL(t *testing.T) {
	root := writeConfig(t, testConfig)
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	code, _, _ := run(t, root, "add", "bad", "--api", "://not-a-url")
	if code == 0 {
		t.Fatal("invalid URL accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("file mutated on failed validation")
	}
}

func TestRemoveRefusesReferencedProvider(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, _, _ := run(t, root, "remove", "fake")
	if code == 0 {
		t.Fatal("referenced provider removal accepted")
	}
}

func TestRejectsUnknownCommand(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "frobnicate"); code != exitUsage {
		t.Fatalf("unknown = %d, want usage", code)
	}
}
