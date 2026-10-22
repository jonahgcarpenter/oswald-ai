package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func browserConfigFixture(t *testing.T, tools string) string {
	t.Helper()
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-process-credential")
	text := syntheticProfileConfig + "tools:\n" + tools
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "api"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBrowserConfigDefaults(t *testing.T) {
	root := profileConfigFixture(t, syntheticProfileConfig)
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrowserLaunchCommand != "" || cfg.BrowserEndpointURL != "" {
		t.Fatalf("browser defaults = %+v", cfg)
	}
	if cfg.BrowserCommandTimeout != 30*time.Second || cfg.BrowserSnapshotThreshold != 15000 || cfg.BrowserInactivityTimeout != 2*time.Minute {
		t.Fatalf("browser timeout defaults = %+v", cfg)
	}
	if cfg.BrowserRecordSessions || cfg.BrowserAllowPrivateURLs || cfg.BrowserAllowInsecureURLs {
		t.Fatal("browser safety defaults must be fail-closed")
	}
}

func TestBrowserConfigParses(t *testing.T) {
	root := browserConfigFixture(t, "  browser:\n    endpoint_url: \"http://127.0.0.1:9222\"\n    command_timeout: 45s\n    snapshot_threshold: 20000\n    inactivity_timeout: 5m\n    record_sessions: true\n    allow_private_urls: true\n    allow_insecure_urls: true\n    launch_command: \"chromium --no-sandbox\"\n")
	cfg, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrowserEndpointURL != "http://127.0.0.1:9222" || cfg.BrowserCommandTimeout != 45*time.Second || cfg.BrowserSnapshotThreshold != 20000 || cfg.BrowserInactivityTimeout != 5*time.Minute || !cfg.BrowserRecordSessions || !cfg.BrowserAllowPrivateURLs || !cfg.BrowserAllowInsecureURLs || cfg.BrowserLaunchCommand != "chromium --no-sandbox" {
		t.Fatalf("browser config = %+v", cfg)
	}
}

func TestBrowserConfigRejectsInvalid(t *testing.T) {
	for _, tools := range []string{
		"  browser:\n    endpoint_url: \"ftp://example.com\"\n",
		"  browser:\n    endpoint_url: \"localhost:9222\"\n",
		"  browser:\n    command_timeout: 1s\n",
		"  browser:\n    command_timeout: nonsense\n",
		"  browser:\n    snapshot_threshold: 10\n",
		"  browser:\n    inactivity_timeout: 1s\n",
	} {
		root := browserConfigFixture(t, tools)
		_, err := LoadProfiles(root)
		if err == nil {
			t.Fatalf("invalid browser config accepted: %q", tools)
		}
		if msg := err.Error(); strings.Contains(msg, "9222") || strings.Contains(msg, "nonsense") {
			t.Fatalf("config content leaked: %v", err)
		}
	}
}
