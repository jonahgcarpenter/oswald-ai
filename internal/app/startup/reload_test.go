package startup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/profiles"
)

const reloadFixture = `providers:
  fake:
    api: http://localhost:9999/v1
    key: test-key
model:
  provider: custom:fake
  default: fake/model
  context_length: 32768
platforms:
  discord:
    enabled: true
    extra:
      token: test-token
      allowed_users: ["111"]
      banned_users: []
runtime:
  log_level: info
`

func reloadRoot(t *testing.T, text string) string {
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

func reloadSetup(t *testing.T, text string) (string, *profiles.Directory, *config.Logger, *strings.Builder) {
	t.Helper()
	root := reloadRoot(t, text)
	cfg, err := config.Validate(root)
	if err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	var output strings.Builder
	log := config.NewLogger(config.LevelDebug)
	log.SetOutput(&output)
	directory, err := profiles.NewDirectory(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	return root, directory, log, &output
}

func rewrite(t *testing.T, root, old, new string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), old) {
		t.Fatalf("fixture missing %q", old)
	}
	if err := config.AtomicWritePrivateFile(filepath.Join(root, "config.yaml"), []byte(strings.Replace(string(data), old, new, 1))); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyReloadMatrix(t *testing.T) {
	base, err := config.Validate(reloadRoot(t, reloadFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		text  string
		hot   bool
		class string
	}{
		{"identical", reloadFixture, true, ""},
		{"log level", strings.Replace(reloadFixture, "log_level: info", "log_level: debug", 1), true, "runtime"},
		{"admission", strings.Replace(reloadFixture, `["111"]`, `["222"]`, 1), true, "admission"},
		{"model", strings.Replace(reloadFixture, "default: fake/model", "default: fake/other", 1), false, "model"},
		{"token", strings.Replace(reloadFixture, "token: test-token", "token: other-token", 1), false, "platforms"},
		{"mcp", reloadFixture + "mcp_servers:\n  notes:\n    url: https://example.com/mcp\n", false, "mcp_servers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := config.Validate(reloadRoot(t, tc.text))
			if err != nil {
				t.Fatalf("variant invalid: %v", err)
			}
			hot, classes := classifyReload(base, next)
			if hot != tc.hot {
				t.Fatalf("hot=%v want %v (classes=%v)", hot, tc.hot, classes)
			}
			if tc.class != "" {
				found := false
				for _, class := range classes {
					if class == tc.class {
						found = true
					}
				}
				if !found {
					t.Fatalf("class %q missing in %v", tc.class, classes)
				}
			}
		})
	}
}

func TestReloaderAppliesHotAdmissionChange(t *testing.T) {
	// Debug level keeps the applied telemetry visible; the reload from info
	// would correctly filter its own DEBUG event under the new level.
	root, directory, log, output := reloadSetup(t, strings.Replace(reloadFixture, "log_level: info", "log_level: debug", 1))
	loader := NewReloader(root, mustValidate(t, root), directory, log, nil)
	loader.settle = 10 * time.Millisecond
	if _, err := directory.Resolve("discord", "111", true); err != nil {
		t.Fatalf("fixture identity rejected: %v", err)
	}
	rewrite(t, root, `["111"]`, `["222"]`)
	loader.Check()
	if _, err := directory.Resolve("discord", "222", true); err != nil {
		t.Fatal("hot admission change did not take effect")
	}
	if _, err := directory.Resolve("discord", "111", true); err == nil {
		t.Fatal("revoked identity still admitted")
	}
	if !strings.Contains(output.String(), `"event":"config.reload.applied"`) {
		t.Fatalf("applied event missing: %s", output.String())
	}
}

func TestReloaderAppliesLogLevel(t *testing.T) {
	root, directory, log, _ := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log, nil)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "log_level: info", "log_level: debug")
	loader.Check()
	if log.Level() != config.LevelDebug {
		t.Fatal("log level change did not take effect")
	}
}

func TestReloaderRejectsInvalidCandidate(t *testing.T) {
	root, directory, log, output := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log, nil)
	loader.settle = 10 * time.Millisecond
	if err := config.AtomicWritePrivateFile(filepath.Join(root, "config.yaml"), []byte("platforms: []\n")); err != nil {
		t.Fatal(err)
	}
	loader.Check()
	if _, err := directory.Resolve("discord", "111", true); err != nil {
		t.Fatal("invalid candidate displaced live config")
	}
	if !strings.Contains(output.String(), `"event":"config.reload.rejected"`) {
		t.Fatalf("rejected event missing: %s", output.String())
	}
	if strings.Contains(output.String(), "test-token") {
		t.Fatal("secret leaked in reload log")
	}
}

func TestReloaderFlagsRestartRequired(t *testing.T) {
	root, directory, log, output := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log, nil)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "default: fake/model", "default: fake/other")
	loader.Check()
	profile, ok := directory.Config("default")
	if !ok || profile.LLMGatewayModel != "fake/model" {
		t.Fatal("restart-required change went live without a restart")
	}
	if !strings.Contains(output.String(), `"event":"config.reload.restart_required"`) {
		t.Fatalf("restart_required event missing: %s", output.String())
	}
}

func TestReloaderStartStopJoins(t *testing.T) {
	root, directory, log, _ := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log, nil)
	loader.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	loader.Start(ctx)
	cancel()
	loader.Stop()
	// Second stop must not block or panic.
	loader.Stop()
	_ = io.Discard
}

func TestReloaderRequestsRebuildOncePerStableChange(t *testing.T) {
	root, directory, log, _ := reloadSetup(t, reloadFixture)
	rebuild := make(chan struct{}, 1)
	loader := NewReloader(root, mustValidate(t, root), directory, log, rebuild)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "default: fake/model", "default: fake/other")
	loader.Check()
	select {
	case <-rebuild:
	default:
		t.Fatal("validated non-hot change did not request a rebuild")
	}
	// Unchanged fingerprint: no second signal.
	loader.Check()
	select {
	case <-rebuild:
		t.Fatal("stable fingerprint requested a second rebuild")
	default:
	}
}

func TestReloaderRebuildSkipsHotAndInvalid(t *testing.T) {
	root, directory, log, _ := reloadSetup(t, reloadFixture)
	rebuild := make(chan struct{}, 1)
	loader := NewReloader(root, mustValidate(t, root), directory, log, rebuild)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "log_level: info", "log_level: debug")
	loader.Check()
	select {
	case <-rebuild:
		t.Fatal("hot change requested a rebuild")
	default:
	}
	if err := config.AtomicWritePrivateFile(filepath.Join(root, "config.yaml"), []byte("platforms: []\n")); err != nil {
		t.Fatal(err)
	}
	loader.Check()
	select {
	case <-rebuild:
		t.Fatal("invalid candidate requested a rebuild")
	default:
	}
}

func mustValidate(t *testing.T, root string) *config.Config {
	t.Helper()
	cfg, err := config.Validate(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
