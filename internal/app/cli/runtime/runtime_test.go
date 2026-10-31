package runtime

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

func TestShowDisplaysLevel(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "show")
	if code != 0 || !strings.Contains(out, "info") {
		t.Fatalf("show = %d %q", code, out)
	}
}

func TestSetRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set", "--log-level", "debug")
	if code != 0 {
		t.Fatalf("set = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "runtime.log_level") || !strings.Contains(out, "applied live") {
		t.Fatalf("stdout missing live hint = %q", out)
	}
	code, out, _ = run(t, root, "show")
	if code != 0 || !strings.Contains(out, "debug") {
		t.Fatalf("show after set = %d %q", code, out)
	}
}

func TestSetRejectsUnknownLevel(t *testing.T) {
	root := writeConfig(t, testConfig)
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	code, _, _ := run(t, root, "set", "--log-level", "verbose")
	if code != exitUsage {
		t.Fatalf("unknown level = %d, want usage", code)
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("file mutated on rejected level")
	}
}

func TestSetRejectsMissingFlag(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set"); code != exitUsage {
		t.Fatalf("bare set = %d, want usage", code)
	}
}

func TestRejectsUnknownCommand(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "frobnicate"); code != exitUsage {
		t.Fatalf("unknown = %d, want usage", code)
	}
}

func TestNamedScopeReadsAllowedMutationsDenied(t *testing.T) {
	root := writeConfig(t, testConfig)
	if err := os.MkdirAll(filepath.Join(root, "profiles", "bob"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", filepath.Join(root, "profiles", "bob"))
	if code, _, _ := run(t, root, "show"); code != 0 {
		t.Fatal("named-scope show rejected")
	}
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if code, _, _ := run(t, root, "set", "--log-level", "debug"); code == 0 {
		t.Fatal("named-scope set accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if string(before) != string(after) {
		t.Fatal("global config mutated by rejected runtime command")
	}
}
