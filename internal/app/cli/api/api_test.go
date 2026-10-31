package api

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
model:
  provider: custom:fake
  default: fake/model
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
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

func TestSetClearRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set-key", "--key", "0123456789abcdef")
	if code != 0 {
		t.Fatalf("set-key = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "api.key") {
		t.Fatalf("stdout = %q", out)
	}
	code, out, _ = run(t, root, "show")
	if code != 0 {
		t.Fatalf("show = %d", code)
	}
	if strings.Contains(out, "0123456789abcdef") {
		t.Fatal("secret leaked in show output")
	}
	code, _, stderr = run(t, root, "clear-key")
	if code != 0 {
		t.Fatalf("clear-key = %d stderr=%s", code, stderr)
	}
}

func TestSetKeyRejectsShortKey(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set-key", "--key", "short"); code == 0 {
		t.Fatal("short key accepted")
	}
	if code, _, _ := run(t, root, "set-key"); code != exitUsage {
		t.Fatalf("missing flag = %d, want usage", code)
	}
}
