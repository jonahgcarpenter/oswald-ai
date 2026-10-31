package model

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
  other:
    api: http://127.0.0.1:9998/v1
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

func TestShowDisplaysModel(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "show")
	if code != 0 || !strings.Contains(out, "fake/model") {
		t.Fatalf("show = %d %q", code, out)
	}
}

func TestSetDefaultRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set", "--default", "fake/other")
	if code != 0 {
		t.Fatalf("set = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "restarting service to apply") {
		t.Fatalf("set missing applying hint = %q", out)
	}
	code, out, _ = run(t, root, "show")
	if code != 0 || !strings.Contains(out, "fake/other") {
		t.Fatalf("show after set = %d %q", code, out)
	}
}

func TestSetProviderSwitch(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, stderr := run(t, root, "set", "--provider", "custom:other"); code != 0 {
		t.Fatalf("provider switch rejected: %s", stderr)
	}
}

func TestSetRejectsUnknownProvider(t *testing.T) {
	root := writeConfig(t, testConfig)
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	code, _, _ := run(t, root, "set", "--provider", "custom:missing")
	if code == 0 {
		t.Fatal("unknown provider accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("file mutated on failed validation")
	}
}

func TestSetRejectsMultipleFields(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set", "--default", "a", "--provider", "custom:fake"); code != exitUsage {
		t.Fatalf("multi-field set = %d, want usage", code)
	}
}
