package process

import (
	"strings"
	"testing"
)

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
