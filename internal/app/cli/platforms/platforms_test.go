package platforms

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
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
  discord:
    enabled: true
    extra:
      token: test-token
      banned_users: []
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

func TestShowRedactsSecrets(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, _ := run(t, root, "show", "discord")
	if code != 0 {
		t.Fatalf("show = %d", code)
	}
	if strings.Contains(out, "test-token") {
		t.Fatal("secret leaked in show output")
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatal("redaction marker missing")
	}
}

func TestEnableDisableRoundTrip(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, _, stderr := run(t, root, "disable", "discord")
	if code != 0 {
		t.Fatalf("disable = %d stderr=%s", code, stderr)
	}
	code, _, stderr = run(t, root, "enable", "bluebubbles")
	if code != 0 {
		t.Fatalf("enable = %d stderr=%s", code, stderr)
	}
}

func TestEnableRejectsMissingCredential(t *testing.T) {
	broken := strings.Replace(testConfig, "      token: test-token", `      token: ""`, 1)
	broken = strings.Replace(broken, "  discord:\n    enabled: true", "  discord:\n    enabled: false", 1)
	root := writeConfig(t, broken)
	code, _, stderr := run(t, root, "enable", "discord")
	if code != exitError {
		t.Fatalf("enable without credential = %d, want error", code)
	}
	if !strings.Contains(stderr, "config_credential_missing") {
		t.Fatalf("stderr missing code = %q", stderr)
	}
}

func TestDisableLastGatewayRejected(t *testing.T) {
	onlyAPI := `providers:
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
  discord:
    enabled: false
    extra:
      token: test-token
  bluebubbles:
    enabled: false
    extra:
      server_url: http://127.0.0.1:1234
`
	root := writeConfig(t, onlyAPI)
	if code, _, stderr := run(t, root, "disable", "api"); code != exitError {
		t.Fatalf("disable last = %d stderr=%s, want error", code, stderr)
	}
}

func TestSetAdmissionAppliesLive(t *testing.T) {
	root := writeConfig(t, testConfig)
	code, out, stderr := run(t, root, "set", "discord", "--ban", "999")
	if code != 0 {
		t.Fatalf("set ban = %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "applied live") {
		t.Fatalf("admission change should apply live, got %q", out)
	}
}

func TestSetRejectsUnknownPlatform(t *testing.T) {
	root := writeConfig(t, testConfig)
	if code, _, _ := run(t, root, "set", "frobnicate", "--token", "x"); code != exitUsage {
		t.Fatalf("unknown = %d, want usage", code)
	}
	if code, _, _ := run(t, root, "enable", "frobnicate"); code != exitUsage {
		t.Fatalf("enable unknown = %d, want usage", code)
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
	if code, _, _ := run(t, root, "enable", "bluebubbles"); code == 0 {
		t.Fatal("named-scope enable accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if string(before) != string(after) {
		t.Fatal("global config mutated by rejected platforms command")
	}
}
