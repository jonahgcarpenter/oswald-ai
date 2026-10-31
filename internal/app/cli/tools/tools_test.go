package tools

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
tools:
  web_search:
    searxng_url: http://127.0.0.1:8888
  browser:
    launch_command: "chromium --no-sandbox"
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

func TestShowAllAndBackend(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "show")
	if code != 0 || !strings.Contains(out, "web_search") {
		t.Fatalf("show = %d %q", code, out)
	}
	code, out, _ = run(t, root, "show", "browser")
	if code != 0 || !strings.Contains(out, "chromium") {
		t.Fatalf("show browser = %d %q", code, out)
	}
}

func TestSetWebSearchRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set", "web-search", "--searxng-url", "http://127.0.0.1:9999")
	if code != 0 {
		t.Fatalf("set = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "tools.web_search") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestSetImageRejectsBadURL(t *testing.T) {
	root := writeConfig(t, testConfig)
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	code, _, _ := run(t, root, "set", "image", "--url", "://bad")
	if code == 0 {
		t.Fatal("bad URL accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("file mutated on failed validation")
	}
}

func TestSetBrowserBoolValidation(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set", "browser", "--record-sessions", "maybe"); code != exitUsage {
		t.Fatalf("bad bool = %d, want usage", code)
	}
	code, _, stderr := run(t, root, "set", "browser", "--record-sessions", "true")
	if code != 0 {
		t.Fatalf("bool set rejected: %s", stderr)
	}
}

func TestUnsetRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, _, stderr := run(t, root, "unset", "web-search.searxng-url")
	if code != 0 {
		t.Fatalf("unset = %d stderr=%s", code, stderr)
	}
}

func TestRejectsUnknownBackend(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set", "frobnicate", "--url", "http://x"); code != exitUsage {
		t.Fatalf("unknown backend = %d, want usage", code)
	}
	if code, _, _ := run(t, root, "show", "frobnicate"); code != exitUsage {
		t.Fatalf("unknown show = %d, want usage", code)
	}
}

func TestProfileFlagAnyPosition(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, _, _ := run(t, root, "--profile", "default", "show")
	if code != 0 {
		t.Fatalf("profile-first show = %d", code)
	}
	code, _, _ = run(t, root, "show", "--profile", "default")
	if code != 0 {
		t.Fatalf("profile-last show = %d", code)
	}
}

func TestCrossProfileReadAllowedMutateDenied(t *testing.T) {
	root := writeConfig(t, testConfig)
	if err := os.MkdirAll(filepath.Join(root, "profiles", "bob"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "profiles", "bob", "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OSWALD_HOME", filepath.Join(root, "profiles", "bob"))
	code, out, _ := run(t, root, "show", "--profile", "default")
	if code != 0 || !strings.Contains(out, "web_search") {
		t.Fatalf("cross-profile read rejected: %d %q", code, out)
	}
	before, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if code, _, _ := run(t, root, "set", "--profile", "default", "web-search", "--searxng-url", "http://127.0.0.1:9999"); code == 0 {
		t.Fatal("cross-profile set accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("default config mutated by rejected set")
	}
}
