package config

import (
	"os"
	"path/filepath"
	"testing"
)

func delegationFixture(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("PROFILE_TEST_MODEL_KEY", "synthetic-process-credential")
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "api"} {
		if err := os.MkdirAll(filepath.Join(root, "profiles", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	full := syntheticProfileConfig + body
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(full), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDelegationDefaultsWhenAbsent(t *testing.T) {
	root := delegationFixture(t, "")
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.DelegationModel != "" || cfg.DelegationProvider != "" || cfg.DelegationBaseURL != "" {
		t.Fatalf("expected empty routing, got %+v", cfg)
	}
	if cfg.DelegationMaxIterations != 250 || cfg.DelegationMaxConcurrentChildren != 10 {
		t.Fatalf("bad iteration defaults: %+v", cfg)
	}
	if cfg.DelegationMaxSpawnDepth != 1 || !cfg.DelegationOrchestratorEnabled {
		t.Fatalf("bad nesting defaults: %+v", cfg)
	}
	got, ok := cfg.ProviderEndpoints["fake"]
	if !ok || got.URL == "" {
		t.Fatalf("missing fake endpoint: %+v", cfg.ProviderEndpoints)
	}
}

func TestDelegationFullSection(t *testing.T) {
	root := delegationFixture(t, "delegation:\n  model: \"openrouter/fast-model\"\n  provider: fake\n  base_url: https://delegate.example.com/v1\n  max_iterations: 40\n  max_concurrent_children: 3\n  max_spawn_depth: 2\n  orchestrator_enabled: false\n")
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.DelegationModel != "openrouter/fast-model" || cfg.DelegationProvider != "fake" {
		t.Fatalf("routing not applied: %+v", cfg)
	}
	if cfg.DelegationBaseURL != "https://delegate.example.com/v1" {
		t.Fatalf("base url not applied: %q", cfg.DelegationBaseURL)
	}
	if cfg.DelegationMaxIterations != 40 || cfg.DelegationMaxConcurrentChildren != 3 {
		t.Fatalf("bounds not applied: %+v", cfg)
	}
	if cfg.DelegationMaxSpawnDepth != 2 || cfg.DelegationOrchestratorEnabled {
		t.Fatalf("nesting not applied: %+v", cfg)
	}
}

func TestDelegationRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"unknown provider", "delegation:\n  provider: ghost\n"},
		{"base url without provider", "delegation:\n  base_url: https://delegate.example.com\n"},
		{"bad base url", "delegation:\n  provider: fake\n  base_url: http://[::1\n"},
		{"non-http base url", "delegation:\n  provider: fake\n  base_url: file:///tmp/x\n"},
		{"zero iterations", "delegation:\n  max_iterations: 0\n"},
		{"huge iterations", "delegation:\n  max_iterations: 10001\n"},
		{"zero children", "delegation:\n  max_concurrent_children: 0\n"},
		{"many children", "delegation:\n  max_concurrent_children: 33\n"},
		{"zero depth", "delegation:\n  max_spawn_depth: 0\n"},
		{"deep depth", "delegation:\n  max_spawn_depth: 9\n"},
		{"unknown key", "delegation:\n  model: x\n  timeout: 5s\n"},
		{"blank model", "delegation:\n  model: \"   \"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := delegationFixture(t, tc.body)
			if _, err := Validate(root); err == nil {
				t.Fatalf("accepted %q", tc.body)
			}
		})
	}
}

func TestDelegationPerProfileOverride(t *testing.T) {
	root := delegationFixture(t, "")
	if err := os.WriteFile(filepath.Join(root, "profiles", "alice", "config.yaml"), []byte("delegation:\n  model: alice/worker\n  max_iterations: 25\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Validate(root)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.DelegationModel != "" {
		t.Fatalf("default profile should mirror, got %q", cfg.DelegationModel)
	}
	alice := cfg.Profiles["alice"]
	if alice == nil {
		t.Fatal("missing alice profile")
	}
	if alice.DelegationModel != "alice/worker" || alice.DelegationMaxIterations != 25 {
		t.Fatalf("alice override not applied: %+v", alice)
	}
	if alice.DelegationMaxConcurrentChildren != 10 {
		t.Fatalf("alice should inherit sibling defaults: %+v", alice)
	}
}

func TestDelegationReloadIsRestart(t *testing.T) {
	oldCfg, err := Validate(delegationFixture(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	nextCfg, err := Validate(delegationFixture(t, "delegation:\n  max_iterations: 30\n"))
	if err != nil {
		t.Fatal(err)
	}
	hot, classes := ClassifyReload(oldCfg, nextCfg)
	if hot {
		t.Fatal("delegation change applied live; want restart")
	}
	found := false
	for _, class := range classes {
		if class == "delegation" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing delegation class in %v", classes)
	}
}
