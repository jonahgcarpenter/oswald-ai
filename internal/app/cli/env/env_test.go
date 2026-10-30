package env

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
    key: ${TEST_ENV_KEY}
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
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TEST_ENV_KEY=initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func run(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestShowListsNamesOnly(t *testing.T) {
	root := testRoot(t)
	code, out, _ := run(t, root, "show")
	if code != 0 {
		t.Fatalf("show failed: %d", code)
	}
	if !strings.Contains(out, "TEST_ENV_KEY") {
		t.Fatalf("name missing: %q", out)
	}
	if strings.Contains(out, "initial") {
		t.Fatal("value leaked in show output")
	}
}

func TestSetUnsetRoundTrip(t *testing.T) {
	root := testRoot(t)
	if code, _, stderr := run(t, root, "set", "TEST_ENV_NEW", "s3cret"); code != 0 {
		t.Fatalf("set failed: %s", stderr)
	}
	code, out, _ := run(t, root, "show")
	if code != 0 || !strings.Contains(out, "TEST_ENV_NEW") {
		t.Fatalf("new var missing: %d %q", code, out)
	}
	if strings.Contains(out, "s3cret") {
		t.Fatal("value leaked")
	}
	if code, _, stderr := run(t, root, "unset", "TEST_ENV_NEW"); code != 0 {
		t.Fatalf("unset failed: %s", stderr)
	}
	code, out, _ = run(t, root, "show")
	if code != 0 || strings.Contains(out, "TEST_ENV_NEW") {
		t.Fatalf("unset did not remove: %d %q", code, out)
	}
}

func TestSetInvalidNameRejected(t *testing.T) {
	root := testRoot(t)
	if code, _, _ := run(t, root, "set", "not-a-name!", "x"); code == 0 {
		t.Fatal("invalid name accepted")
	}
}
