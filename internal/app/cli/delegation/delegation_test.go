package delegation

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

func TestShowEmptySection(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "show")
	if code != 0 || !strings.Contains(out, "mirror the profile model") {
		t.Fatalf("show = %d %q", code, out)
	}
}

func TestSetModelRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set", "--model", "other/worker")
	if code != 0 {
		t.Fatalf("set = %d %s", code, stderr)
	}
	if !strings.Contains(out, "restart required - run `oswald restart` to apply") {
		t.Fatalf("set missing applying hint = %q", out)
	}
	code, out, _ = run(t, root, "show")
	if code != 0 || !strings.Contains(out, "other/worker") {
		t.Fatalf("show after set = %d %q", code, out)
	}
}

func TestSetProviderAndUnset(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, stderr := run(t, root, "set", "--provider", "other"); code != 0 {
		t.Fatalf("set provider = %d %s", code, stderr)
	}
	if code, _, stderr := run(t, root, "set", "--provider", "ghost"); code == 0 {
		t.Fatalf("unknown provider accepted: %s", stderr)
	}
	if code, _, stderr := run(t, root, "unset", "provider"); code != 0 {
		t.Fatalf("unset provider = %d %s", code, stderr)
	}
	if code, _, stderr := run(t, root, "unset", "max-iterations"); code == 0 {
		t.Fatalf("unset of numeric leaf accepted: %s", stderr)
	}
}

func TestSetBaseURLNeedsProvider(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set", "--base-url", "https://delegate.example.com"); code == 0 {
		t.Fatal("base-url without provider accepted")
	}
	if code, _, stderr := run(t, root, "set", "--provider", "other"); code != 0 {
		t.Fatalf("set provider = %d %s", code, stderr)
	}
	if code, _, stderr := run(t, root, "set", "--base-url", "https://delegate.example.com/v1"); code != 0 {
		t.Fatalf("set base-url = %d %s", code, stderr)
	}
}

func TestSetBoundsAndUsage(t *testing.T) {
	root := writeConfig(t, testConfig)
	for _, args := range [][]string{
		{"set", "--max-iterations", "0"},
		{"set", "--max-children", "33"},
		{"set", "--max-depth", "0"},
		{"set", "--orchestrator", "maybe"},
		{"set", "--model", "a", "--provider", "other"},
		{"set"},
		{"unset"},
		{"unset", "bogus"},
		{"bogus"},
	} {
		if code, _, _ := run(t, root, args...); code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
	if code, _, stderr := run(t, root, "set", "--max-iterations", "40"); code != 0 {
		t.Fatalf("set max-iterations = %d %s", code, stderr)
	}
	if code, _, stderr := run(t, root, "set", "--orchestrator", "false"); code != 0 {
		t.Fatalf("set orchestrator = %d %s", code, stderr)
	}
}

func TestHelpPages(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		code, out, _ := run(t, root, args...)
		if code != exitOK || !strings.Contains(out, "usage: oswald delegation") {
			t.Fatalf("%s %v = %d %q, want exit 0 with group usage", "delegation", args, code, out)
		}
	}
	for _, leaf := range []string{"show", "set", "unset"} {
		code, out, _ := run(t, root, leaf, "--help")
		if code != exitOK || !strings.Contains(out, "usage: oswald delegation "+leaf) {
			t.Fatalf("delegation %s --help = %d %q, want exit 0 with leaf usage", leaf, code, out)
		}
	}
}
