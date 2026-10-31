package gateway

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
    api: "http://127.0.0.1:9999/v1"
model:
  provider: custom:fake
  default: fake/model
gateway:
  multiplex_profiles: true
  profile_routes: []
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
  discord:
    enabled: true
    extra:
      token: test-token
  bluebubbles:
    enabled: false
    extra:
      server_url: http://127.0.0.1:1234
      server_password: test-password
      webhook_port: 8090
`

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(testConfig), 0600); err != nil {
		t.Fatal(err)
	}
	_ = text
	return root
}

func run(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), root, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestShowDisplaysGateway(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "show")
	if code != 0 || !strings.Contains(out, "multiplex_profiles") {
		t.Fatalf("show = %d %q", code, out)
	}
}

func TestSetMultiplexRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set-multiplex", "false")
	if code != 0 {
		t.Fatalf("set-multiplex = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "multiplex_profiles") {
		t.Fatalf("stdout = %q", out)
	}
	code, out, _ = run(t, root, "show")
	if code != 0 || !strings.Contains(out, "false") {
		t.Fatalf("show after set = %d %q", code, out)
	}
}

func TestSetMultiplexRejectsBadBool(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set-multiplex", "maybe"); code != exitUsage {
		t.Fatalf("bad bool = %d, want usage", code)
	}
}

func TestRoutesAddRemoveRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "routes", "add", "--name", "r1", "--platform", "discord", "--user-id", "111", "--profile", "default")
	if code != 0 {
		t.Fatalf("routes add = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "r1") {
		t.Fatalf("stdout = %q", out)
	}
	code, out, _ = run(t, root, "routes", "list")
	if code != 0 || !strings.Contains(out, "r1") {
		t.Fatalf("routes list = %d %q", code, out)
	}
	code, _, stderr = run(t, root, "routes", "remove", "r1")
	if code != 0 {
		t.Fatalf("routes remove = %d stderr=%s", code, stderr)
	}
}

func TestRoutesAddRejectsUnknownPlatform(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "routes", "add", "--name", "r1", "--platform", "imessage", "--user-id", "111", "--profile", "default"); code != exitUsage {
		t.Fatalf("bad platform = %d, want usage", code)
	}
}

func TestRoutesRemoveUnknown(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "routes", "remove", "missing"); code != exitError {
		t.Fatalf("remove missing = %d, want error", code)
	}
}

func TestRetiredSpellingRedirects(t *testing.T) {
	root := writeConfig(t, testConfig)
	for _, args := range [][]string{{"discord", "enable"}, {"api", "disable"}} {
		code, _, stderr := run(t, root, args...)
		if code != exitUsage {
			t.Fatalf("retired %v = %d, want usage", args, code)
		}
		if !strings.Contains(stderr, "oswald platforms") {
			t.Fatalf("stderr missing redirect = %q", stderr)
		}
	}
}
