package startup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		{"model", strings.Replace(reloadFixture, "default: fake/model", "default: fake/other", 1), true, "model"},
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
	loader := NewReloader(root, mustValidate(t, root), directory, log)
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
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "log_level: info", "log_level: debug")
	loader.Check()
	if log.Level() != config.LevelDebug {
		t.Fatal("log level change did not take effect")
	}
}

func TestReloaderAppliesHotModelChange(t *testing.T) {
	// Debug level keeps the applied telemetry visible after the swap retunes
	// the shared level to the candidate's value.
	root, directory, log, output := reloadSetup(t, strings.Replace(reloadFixture, "log_level: info", "log_level: debug", 1))
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	var calls int
	var callOld, callNext *config.Config
	var swappedModel, lastModel string
	loader.SetModelApplier(func(old, next *config.Config) {
		calls++
		callOld, callNext = old, next
		if profile, ok := directory.Config("default"); ok {
			swappedModel = profile.LLMGatewayModel
		}
		if loader.last != nil {
			lastModel = loader.last.LLMGatewayModel
		}
	})
	rewrite(t, root, "default: fake/model", "default: fake/other")
	loader.Check()
	if calls != 1 {
		t.Fatalf("model applier calls = %d, want 1", calls)
	}
	if callOld == nil || callOld.LLMGatewayModel != "fake/model" {
		t.Fatalf("applier old config = %+v", callOld)
	}
	if callNext == nil || callNext.LLMGatewayModel != "fake/other" {
		t.Fatalf("applier next config = %+v", callNext)
	}
	if swappedModel != "fake/other" {
		t.Fatal("applier ran before the Directory swap")
	}
	if lastModel != "fake/model" {
		t.Fatal("applier ran after the reloader advanced its snapshot")
	}
	profile, ok := directory.Config("default")
	if !ok || profile.LLMGatewayModel != "fake/other" {
		t.Fatal("hot model change did not take effect")
	}
	if loader.last == nil || loader.last.LLMGatewayModel != "fake/other" {
		t.Fatal("reloader did not advance to the applied config")
	}
	if !strings.Contains(output.String(), `"event":"config.reload.applied"`) {
		t.Fatalf("applied event missing: %s", output.String())
	}
}

func TestReloaderAppliesHotModelChangeWithoutApplier(t *testing.T) {
	root, directory, log, output := reloadSetup(t, strings.Replace(reloadFixture, "log_level: info", "log_level: debug", 1))
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "default: fake/model", "default: fake/other")
	loader.Check()
	profile, ok := directory.Config("default")
	if !ok || profile.LLMGatewayModel != "fake/other" {
		t.Fatal("hot model change did not swap without an applier")
	}
	if !strings.Contains(output.String(), `"event":"config.reload.applied"`) {
		t.Fatalf("applied event missing: %s", output.String())
	}
}

func TestReloaderSkipsApplierForWindowOnlyChange(t *testing.T) {
	root, directory, log, output := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	calls := 0
	loader.SetModelApplier(func(_, _ *config.Config) { calls++ })
	rewrite(t, root, "context_length: 32768", "context_length: 65536")
	loader.Check()
	if calls != 0 {
		t.Fatalf("model applier calls = %d, want 0 for a window-only diff", calls)
	}
	if !strings.Contains(output.String(), `"event":"config.reload.restart_required"`) {
		t.Fatalf("restart_required event missing: %s", output.String())
	}
	profile, ok := directory.Config("default")
	if !ok || profile.LLMGatewayModel != "fake/model" {
		t.Fatal("window-only change displaced the live model")
	}
}

func TestReloaderRejectsInvalidCandidate(t *testing.T) {
	root, directory, log, output := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log)
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
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "context_length: 32768", "context_length: 65536")
	loader.Check()
	profile, ok := directory.Config("default")
	if !ok || profile.LLMGatewayModel != "fake/model" {
		t.Fatal("restart-required change went live without a restart")
	}
	if profile.ModelContextWindow != 32768 {
		t.Fatal("restart-required window change went live without a restart")
	}
	if !strings.Contains(output.String(), `"event":"config.reload.restart_required"`) {
		t.Fatalf("restart_required event missing: %s", output.String())
	}
}

func TestReloaderSetModelApplierConcurrentWithCheck(t *testing.T) {
	root, directory, log, _ := reloadSetup(t, strings.Replace(reloadFixture, "log_level: info", "log_level: debug", 1))
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "default: fake/model", "default: fake/other")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				loader.SetModelApplier(func(_, _ *config.Config) {})
			}
		}
	}()
	loader.Check()
	close(stop)
	wg.Wait()
	if profile, ok := directory.Config("default"); !ok || profile.LLMGatewayModel != "fake/other" {
		t.Fatal("hot model change did not survive concurrent attach")
	}
}

func TestReloaderStartStopJoins(t *testing.T) {
	root, directory, log, _ := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	loader.Start(ctx)
	cancel()
	loader.Stop()
	// Second stop must not block or panic.
	loader.Stop()
	_ = io.Discard
}

func TestReloaderAdvisesRestartOncePerStableChange(t *testing.T) {
	root, directory, log, output := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "context_length: 32768", "context_length: 65536")
	loader.Check()
	if !strings.Contains(output.String(), `"event":"config.reload.restart_required"`) {
		t.Fatal("validated non-hot change did not log restart advisory")
	}
	// Live config stays put until `oswald restart`.
	profile, ok := directory.Config("default")
	if !ok || profile.LLMGatewayModel != "fake/model" {
		t.Fatal("non-hot change went live without a restart")
	}
	if profile.ModelContextWindow != 32768 {
		t.Fatal("non-hot window change went live without a restart")
	}
	// Unchanged fingerprint: no second advisory.
	before := output.String()
	loader.Check()
	if output.String() != before {
		t.Fatal("stable fingerprint logged a second advisory")
	}
}

func TestReloaderAdvisorySkipsHotAndInvalid(t *testing.T) {
	root, directory, log, output := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	rewrite(t, root, "log_level: info", "log_level: debug")
	loader.Check()
	if strings.Contains(output.String(), `"event":"config.reload.restart_required"`) {
		t.Fatal("hot change logged a restart advisory")
	}
	if err := config.AtomicWritePrivateFile(filepath.Join(root, "config.yaml"), []byte("platforms: []\n")); err != nil {
		t.Fatal(err)
	}
	loader.Check()
	if strings.Contains(output.String(), `"event":"config.reload.restart_required"`) {
		t.Fatal("invalid candidate logged a restart advisory")
	}
	if !strings.Contains(output.String(), `"event":"config.reload.rejected"`) {
		t.Fatal("invalid candidate did not log rejected")
	}
}

func TestReloaderAdvisesOnPluginInstall(t *testing.T) {
	root, directory, log, output := reloadSetup(t, reloadFixture)
	loader := NewReloader(root, mustValidate(t, root), directory, log)
	loader.settle = 10 * time.Millisecond
	// Simulate `plugins install`: new code tree with manifest + metadata.
	codeDir := filepath.Join(root, "plugins", "demo", "code")
	if err := os.MkdirAll(codeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codeDir, "plugin.yaml"), []byte("name: demo\nversion: \"1.0\"\ndescription: synthetic\nmanifest_version: 2\napi_version: \"1\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codeDir, ".install-metadata.json"), []byte(`{"source":"git","url":"https://example.com/demo.git"}`), 0600); err != nil {
		t.Fatal(err)
	}
	loader.Check()
	if !strings.Contains(output.String(), `"event":"config.reload.restart_required"`) {
		t.Fatal("plugin install did not log restart advisory")
	}
	// Simulate a sidecar appearing (healed deps): marker changes, config identical.
	if err := os.MkdirAll(filepath.Join(codeDir, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	loader.Check()
	if count := strings.Count(output.String(), `"event":"config.reload.restart_required"`); count != 2 {
		t.Fatalf("node sidecar install advisories = %d, want 2", count)
	}
	// Derived shims never trigger an advisory (no fingerprint loop).
	before := output.String()
	if err := os.MkdirAll(filepath.Join(root, "plugins", ".bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugins", ".bin", "demo"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	loader.Check()
	if output.String() != before {
		t.Fatal("shim dir change logged an advisory")
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
