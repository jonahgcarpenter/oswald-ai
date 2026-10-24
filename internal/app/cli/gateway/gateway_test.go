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
platforms:
  api:
    enabled: true
    extra:
      api_port: 12345
  discord:
    enabled: false
    extra:
      token: ""
  bluebubbles:
    enabled: false
    extra:
      server_url: http://127.0.0.1:1234
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

func enabledValue(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	idx := strings.Index(text, name+":")
	if idx < 0 {
		t.Fatalf("gateway %q missing after write", name)
	}
	rest := text[idx:]
	en := strings.Index(rest, "enabled:")
	if en < 0 {
		t.Fatalf("enabled missing for %q", name)
	}
	line := rest[en:]
	end := strings.Index(line, "\n")
	return strings.TrimSpace(line[:end])
}

func TestEnableFlipsFalseToTrue(t *testing.T) {
	root := writeConfig(t, testConfig)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"discord", "enable"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("enable = %d, stderr=%s", code, stderr.String())
	}
	if got := enabledValue(t, root, "discord"); got != "enabled: true" {
		t.Fatalf("discord flag = %q, want enabled: true", got)
	}
	if !strings.Contains(stdout.String(), "gateway discord enabled") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDisableFlipsTrueToFalse(t *testing.T) {
	root := writeConfig(t, testConfig)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"api", "disable"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("disable = %d, stderr=%s", code, stderr.String())
	}
	if got := enabledValue(t, root, "api"); got != "enabled: false" {
		t.Fatalf("api flag = %q, want enabled: false", got)
	}
}

func TestIdempotentAlreadyInState(t *testing.T) {
	root := writeConfig(t, testConfig)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"api", "enable"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("enable = %d", code)
	}
	if !strings.Contains(stdout.String(), "already enabled") {
		t.Fatalf("stdout = %q, want already enabled", stdout.String())
	}
}

func TestRejectsUnknownGatewayAndAliases(t *testing.T) {
	root := writeConfig(t, testConfig)
	for _, name := range []string{"imessage", "IMESSAGE", "Discord", "frobnicate"} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), root, []string{name, "enable"}, &stdout, &stderr); code != exitUsage {
			t.Fatalf("gateway %q = %d, want usage", name, code)
		}
	}
}

func TestRejectsBadActionAndArgCounts(t *testing.T) {
	root := writeConfig(t, testConfig)
	for _, args := range [][]string{nil, {"discord"}, {"discord", "enable", "extra"}, {"discord", "on"}, {"enable", "discord"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), root, args, &stdout, &stderr); code != exitUsage {
			t.Fatalf("args %v = %d, want usage", args, code)
		}
	}
}

func TestMissingFileAndMissingGateway(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"discord", "enable"}, &stdout, &stderr); code != exitError {
		t.Fatalf("missing file = %d, want error", code)
	}
	full := writeConfig(t, testConfig)
	// Remove one platform block: rewrite without bluebubbles.
	trimmed := strings.Replace(testConfig, "  bluebubbles:\n    enabled: false\n    extra:\n      server_url: http://127.0.0.1:1234\n", "", 1)
	if err := os.WriteFile(filepath.Join(full, "config.yaml"), []byte(trimmed), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), full, []string{"bluebubbles", "enable"}, &stdout, &stderr); code != exitError {
		t.Fatalf("missing gateway = %d, want error", code)
	}
}

func TestPreservesOtherPlatforms(t *testing.T) {
	root := writeConfig(t, testConfig)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, []string{"discord", "enable"}, &stdout, &stderr); code != exitOK {
		t.Fatal("enable failed")
	}
	if got := enabledValue(t, root, "api"); got != "enabled: true" {
		t.Fatalf("api flag changed to %q", got)
	}
	if got := enabledValue(t, root, "bluebubbles"); got != "enabled: false" {
		t.Fatalf("bluebubbles flag changed to %q", got)
	}
}
