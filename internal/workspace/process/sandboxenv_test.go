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

func TestWithNodeOnPath(t *testing.T) {
	base := []string{"PATH=/usr/bin:/bin", "HOME=/nowhere"}
	out := WithNodeOnPath(base)
	if len(out) != len(base) || out[1] != "HOME=/nowhere" {
		t.Fatalf("base mutated: %q", out)
	}
	found := false
	for _, entry := range out {
		if strings.HasPrefix(entry, "PATH=") {
			found = true
			if !strings.Contains(entry, "/usr/bin:/bin") {
				t.Fatalf("base PATH lost: %q", entry)
			}
		}
	}
	if !found {
		t.Fatal("PATH entry missing")
	}
	// Idempotent: a second pass never stacks another prefix.
	again := WithNodeOnPath(out)
	if len(again) != len(out) {
		t.Fatalf("not idempotent: %q", again)
	}
}

func TestWithPluginShimsOnPath(t *testing.T) {
	profile := filepath.Join("tmp", ".oswald")
	base := []string{"PATH=/opt/oswald:/usr/bin:/bin", "HOME=/nowhere"}
	out := WithPluginShimsOnPath(WithNodeOnPath(base), profile)
	var pathLine string
	for _, entry := range out {
		if strings.HasPrefix(entry, "PATH=") {
			pathLine = entry
		}
	}
	want := filepath.Join(profile, "plugins", ".bin")
	if !strings.HasSuffix(pathLine, string(os.PathListSeparator)+want) {
		t.Fatalf("shim dir not appended at end: %q", pathLine)
	}
	if !strings.Contains(pathLine, "/opt/oswald:/usr/bin:/bin") {
		t.Fatalf("base PATH lost: %q", pathLine)
	}
	// Idempotent and empty-root safe.
	if again := WithPluginShimsOnPath(out, profile); len(again) != len(out) {
		t.Fatalf("not idempotent: %q", again)
	}
	if same := WithPluginShimsOnPath(base, ""); len(same) != len(base) {
		t.Fatalf("empty root changed base: %q", same)
	}
}

func TestGitHubTokenEnvPassthrough(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-gh-token")
	t.Setenv("GITHUB_TOKEN", "synthetic-github-token")
	t.Setenv("DISCORD_TOKEN", "synthetic-discord-token")

	env := GitHubTokenEnv()
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GH_TOKEN=synthetic-gh-token") {
		t.Fatalf("GH_TOKEN missing from %q", joined)
	}
	if !strings.Contains(joined, "GITHUB_TOKEN=synthetic-github-token") {
		t.Fatalf("GITHUB_TOKEN missing from %q", joined)
	}
	if strings.Contains(joined, "DISCORD_TOKEN") {
		t.Fatalf("non-allowlisted secret leaked into %q", joined)
	}
}

func TestGitHubTokenEnvEmpty(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if env := GitHubTokenEnv(); len(env) != 0 {
		t.Fatalf("expected no entries, got %q", env)
	}
}

func TestSpawnSeesGitHubTokenOnly(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-gh-token")
	t.Setenv("DISCORD_TOKEN", "synthetic-discord-token")

	m := NewManager(t.TempDir())
	info, err := m.Spawn("req-gh", "echo token:$GH_TOKEN; echo other:$DISCORD_TOKEN", t.TempDir(), t.TempDir(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(info.ID, 10); err != nil {
		t.Fatal(err)
	}
	tail, err := m.Tail(info.ID, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tail, "token:synthetic-gh-token") {
		t.Fatalf("GH_TOKEN not visible to background shell: %q", tail)
	}
	if strings.Contains(tail, "synthetic-discord-token") {
		t.Fatalf("non-allowlisted secret visible to background shell: %q", tail)
	}
}
