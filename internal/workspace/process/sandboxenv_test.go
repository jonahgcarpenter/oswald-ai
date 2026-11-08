package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterRefusesCredentials(t *testing.T) {
	ClearEnvPassthrough()
	defer ClearEnvPassthrough()
	refused := RegisterEnvPassthrough([]string{"TENOR_API_KEY", "OPENAI_API_KEY", "DISCORD_TOKEN", "PATH", "OSWALD_HOME", "", "has space"})
	if len(refused) == 0 {
		t.Fatal("expected refusals")
	}
	for _, name := range PassthroughNames() {
		if name != "TENOR_API_KEY" {
			t.Fatalf("unexpected allowlisted name %q", name)
		}
	}
}

func TestScopeMergesDefaultAndProfile(t *testing.T) {
	global := t.TempDir()
	if err := os.Chmod(global, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, ".env"), []byte("SHARED=synthetic-default\nONLY_DEFAULT=synthetic-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(global, "profiles", "bob")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".env"), []byte("SHARED=synthetic-profile\n"), 0600); err != nil {
		t.Fatal(err)
	}
	scope := ProfileSecretScope(profile)
	if scope["SHARED"] != "synthetic-profile" || scope["ONLY_DEFAULT"] != "synthetic-1" {
		t.Fatalf("scope = %+v", scope)
	}
	if got := ProfileSecretScope(filepath.Join(global, "profiles", "missing")); len(got) != 2 {
		t.Fatalf("missing profile scope = %+v", got)
	}
}

func TestResolvePrefersScopeOverProcess(t *testing.T) {
	t.Setenv("SYNTH_PASSTHROUGH_VAR", "synthetic-process")
	scope := map[string]string{"SYNTH_PASSTHROUGH_VAR": "synthetic-scope"}
	value, ok := ResolveEnvValue("SYNTH_PASSTHROUGH_VAR", scope)
	if !ok || value != "synthetic-scope" {
		t.Fatalf("got %q %v", value, ok)
	}
	if _, ok := ResolveEnvValue("OPENAI_API_KEY", map[string]string{"OPENAI_API_KEY": "synthetic"}); ok {
		t.Fatal("refused name resolved")
	}
	if _, ok := ResolveEnvValue("SYNTH_UNSET_PASSTHROUGH_VAR", nil); ok {
		t.Fatal("unset name resolved")
	}
}

func TestSandboxEnvOnlyAllowlisted(t *testing.T) {
	ClearEnvPassthrough()
	defer ClearEnvPassthrough()
	RegisterEnvPassthrough([]string{"SYNTH_ALLOW_VAR", "OPENAI_API_KEY"})
	global := t.TempDir()
	if err := os.Chmod(global, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, ".env"), []byte("SYNTH_ALLOW_VAR=synthetic-allowed\nSYNTH_SECRET_VAR=synthetic-blocked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := SandboxEnv([]string{"PATH=/usr/bin"}, global, []string{"SYNTH_CONFIG_VAR"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "SYNTH_ALLOW_VAR=synthetic-allowed") {
		t.Fatalf("allowlisted value missing: %q", joined)
	}
	if strings.Contains(joined, "SYNTH_SECRET_VAR") || strings.Contains(joined, "OPENAI_API_KEY") || strings.Contains(joined, "SYNTH_CONFIG_VAR=") {
		t.Fatalf("unregistered name leaked: %q", joined)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("base lost: %q", joined)
	}
}
