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
	manifestDir := filepath.Join(root, "plugins-available", "demo")
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		t.Fatal(err)
	}
	body := "name: demo\nversion: \"1.0\"\ndescription: synthetic test plugin\n"
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return root
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
