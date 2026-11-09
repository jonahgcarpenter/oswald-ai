package plugins

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func TestParseInstallSourceShapes(t *testing.T) {
	git, err := ParseInstallSource("acme/widgets")
	if err != nil || git.Kind != "git" || git.URL != "https://github.com/acme/widgets.git" {
		t.Fatalf("shorthand = %+v %v", git, err)
	}
	https, err := ParseInstallSource("https://example.com/org/repo.git")
	if err != nil || https.Kind != "git" {
		t.Fatalf("https = %+v %v", https, err)
	}
	if _, err := ParseInstallSource("http://example.com/org/repo.git"); err == nil {
		t.Fatal("http accepted")
	}
	if _, err := ParseInstallSource("git@example.com:org/repo.git"); err == nil {
		t.Fatal("ssh accepted")
	}
	dir := t.TempDir()
	file, err := ParseInstallSource("file://" + dir)
	if err != nil || file.Kind != "local" || file.URL != dir {
		t.Fatalf("file = %+v %v", file, err)
	}
	for _, bad := range []string{"", "has space/repo", "a/b/c", "../escape", "https://example.com/a b.git"} {
		if _, err := ParseInstallSource(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := ParseInstallSource(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing dir accepted")
	}
}

func TestValidateInstallRefAndSubdir(t *testing.T) {
	if err := ValidateInstallRef(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInstallRef("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"main", "v1.0", "01234567", "0123456789abcdef0123456789abcdef0123456g"} {
		if err := ValidateInstallRef(bad); err == nil {
			t.Fatalf("accepted ref %q", bad)
		}
	}
	if _, err := ValidateInstallSubdir(""); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateInstallSubdir("../escape"); err == nil {
		t.Fatal("escape accepted")
	}
	if _, err := ValidateInstallSubdir("/abs"); err == nil {
		t.Fatal("absolute accepted")
	}
}

func installFixture(t *testing.T, dir, name, extra string) {
	t.Helper()
	body := "name: " + name + "\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\n" + extra
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFromLocalDir(t *testing.T) {
	profile := t.TempDir()
	src := filepath.Join(t.TempDir(), "src-plugin")
	installFixture(t, src, "local-widget", "")
	log := config.NewLogger(config.LevelWarn)
	result, err := InstallFromURL(t.Context(), profile, src, InstallOptions{}, log)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Key != "local-widget" || result.Pinned {
		t.Fatalf("result = %+v", result)
	}
	dest := PluginCodeDir(profile, "local-widget")
	if _, err := os.Stat(filepath.Join(dest, "plugin.yaml")); err != nil {
		t.Fatalf("published files missing: %v", err)
	}
	meta, err := ReadInstallMetadata(profile, "local-widget")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.Source != "local" || meta.URL != src || meta.Pinned {
		t.Fatalf("meta = %+v", meta)
	}
	again, err := InstallFromURL(t.Context(), profile, src, InstallOptions{}, log)
	if err != nil {
		t.Fatalf("idempotent reinstall: %v", err)
	}
	if !again.AlreadyInstalled || again.Key != "local-widget" {
		t.Fatalf("result = %+v", again)
	}
	other := filepath.Join(t.TempDir(), "other-plugin")
	installFixture(t, other, "local-widget", "")
	if _, err := InstallFromURL(t.Context(), profile, other, InstallOptions{}, log); err == nil {
		t.Fatal("conflicting source accepted")
	} else if !strings.Contains(err.Error(), "different source") {
		t.Fatalf("wrong conflict: %v", err)
	}
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{Ref: "0123456789abcdef0123456789abcdef01234567"}, log); err == nil {
		t.Fatal("local ref accepted")
	}
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{Force: true}, log); err != nil {
		t.Fatalf("force reinstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, "plugins-backup")); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func TestInstallRejectsBadManifest(t *testing.T) {
	profile := t.TempDir()
	src := filepath.Join(t.TempDir(), "bad-plugin")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "plugin.yaml"), []byte("name: bad\n"), 0644); err != nil {
		t.Fatal(err)
	}
	log := config.NewLogger(config.LevelWarn)
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{}, log); err == nil {
		t.Fatal("bad manifest accepted")
	}
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{Ref: "main"}, log); err == nil {
		t.Fatal("bad ref accepted")
	}
}

func TestExistingInstallState(t *testing.T) {
	profile := t.TempDir()
	if err := os.MkdirAll(PluginCodeDir(profile, "widgets"), 0755); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/acme/widgets.git"
	rev := "0123456789abcdef0123456789abcdef01234567"
	other := "fedcba9876543210fedcba9876543210fedcba98"
	if err := WriteInstallMetadata(PluginCodeDir(profile, "widgets"),
		InstallMetadata{Source: "git", URL: url, Ref: rev, Revision: rev, Pinned: true}); err != nil {
		t.Fatal(err)
	}
	src := InstallSource{Kind: "git", URL: url}
	if result, done, err := existingInstallState(profile, "widgets", src, "", "", NodeSpec{}); err != nil || !done || !result.AlreadyInstalled {
		t.Fatalf("same source = %+v %v %v", result, done, err)
	}
	if _, done, err := existingInstallState(profile, "widgets", src, "", rev, NodeSpec{}); err != nil || !done {
		t.Fatalf("same ref = %v %v", done, err)
	}
	if _, _, err := existingInstallState(profile, "widgets", src, "", other, NodeSpec{}); err == nil {
		t.Fatal("moved pin accepted")
	} else if !strings.Contains(err.Error(), "update") {
		t.Fatalf("wrong moved-pin error: %v", err)
	}
	otherSrc := InstallSource{Kind: "git", URL: "https://github.com/other/widgets.git"}
	if _, _, err := existingInstallState(profile, "widgets", otherSrc, "", "", NodeSpec{}); err == nil {
		t.Fatal("different source accepted")
	} else if !strings.Contains(err.Error(), "different source") {
		t.Fatalf("wrong source error: %v", err)
	}
	if _, _, err := existingInstallState(profile, "widgets", src, "sub", "", NodeSpec{}); err == nil {
		t.Fatal("different subdir accepted")
	}
	// Hand-placed tree without metadata refuses without --force.
	plain := PluginCodeDir(profile, "plain")
	if err := os.MkdirAll(plain, 0755); err != nil {
		t.Fatal(err)
	}
	plainSrc := filepath.Join(t.TempDir(), "plain")
	installFixture(t, plainSrc, "plain", "")
	if _, err := InstallFromURL(t.Context(), profile, plainSrc, InstallOptions{}, config.NewLogger(config.LevelWarn)); err == nil {
		t.Fatal("unmanaged overwrite accepted")
	} else if !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("wrong unmanaged error: %v", err)
	}
}

func TestRemoveInstallUnknown(t *testing.T) {
	if err := RemoveInstall(t.TempDir(), "ghost", false); err == nil {
		t.Fatal("unknown remove accepted")
	}
	if err := RemoveInstall(t.TempDir(), "nope!", false); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestRemoveInstallKeepsData(t *testing.T) {
	profile := t.TempDir()
	src := filepath.Join(t.TempDir(), "src-plugin")
	installFixture(t, src, "keepme", "")
	log := config.NewLogger(config.LevelWarn)
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{}, log); err != nil {
		t.Fatalf("install: %v", err)
	}
	dataHome := filepath.Join(PluginDataDir(profile, "keepme"), "home")
	if err := os.MkdirAll(dataHome, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dataHome, "session.txt")
	if err := os.WriteFile(marker, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveInstall(profile, "keepme", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(PluginCodeDir(profile, "keepme")); !os.IsNotExist(err) {
		t.Fatal("code not removed")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("data not preserved: %v", err)
	}
	if err := RemoveInstall(profile, "keepme", true); err != nil {
		t.Fatalf("full remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ProfilePluginsDir(profile), "keepme")); !os.IsNotExist(err) {
		t.Fatal("full remove left directory")
	}
}

func TestUpdateFromLocalGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	profile := t.TempDir()
	repo := filepath.Join(t.TempDir(), "plugin-repo")
	manifest := "name: widget\nversion: \"1.0\"\ndescription: synthetic test plugin\nmanifest_version: 2\napi_version: \"1\"\n"
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "plugin.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=synth", "GIT_AUTHOR_EMAIL=synth@example.com", "GIT_COMMITTER_NAME=synth", "GIT_COMMITTER_EMAIL=synth@example.com")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("add", "plugin.yaml")
	runGit("commit", "-qm", "synthetic")
	first, err := gitHead(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	result, err := InstallFromURL(t.Context(), profile, repo, InstallOptions{}, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Revision != first {
		t.Fatalf("revision = %+v want %s", result, first)
	}
	if err := os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("more\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "extra.txt")
	runGit("commit", "-qm", "synthetic two")
	second, err := gitHead(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := UpdateFromURL(t.Context(), profile, "widget", "", nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Revision != second || updated.AlreadyInstalled {
		t.Fatalf("updated = %+v want %s", updated, second)
	}
	if _, err := os.Stat(filepath.Join(PluginCodeDir(profile, "widget"), "extra.txt")); err != nil {
		t.Fatalf("updated tree missing file: %v", err)
	}
	pinned, err := UpdateFromURL(t.Context(), profile, "widget", second, nil)
	if err != nil || !pinned.Pinned {
		t.Fatalf("pin = %+v %v", pinned, err)
	}
	if _, err := UpdateFromURL(t.Context(), profile, "widget", "", nil); err == nil {
		t.Fatal("pinned update without ref accepted")
	}
}

func gitHead(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func TestInstallNodeFlagsRecordedWithoutInstall(t *testing.T) {
	profile := t.TempDir()
	src := filepath.Join(t.TempDir(), "flagged")
	installFixture(t, src, "flagged", "")
	log := config.NewLogger(config.LevelWarn)
	result, err := InstallFromURL(t.Context(), profile, src, InstallOptions{
		NodePackage: "agent-data", NodeVersion: "0.13.0", NoDeps: true,
	}, log)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.DepsInstalled {
		t.Fatalf("deps installed despite --no-deps: %+v", result)
	}
	meta, err := ReadInstallMetadata(profile, "flagged")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.NodePackage != "agent-data" || meta.NodeVersion != "0.13.0" || meta.NodeLockfile {
		t.Fatalf("meta = %+v", meta)
	}
	// Idempotent reinstall with the same flags is a no-op that skips repair.
	again, err := InstallFromURL(t.Context(), profile, src, InstallOptions{
		NodePackage: "agent-data", NodeVersion: "0.13.0", NoDeps: true,
	}, log)
	if err != nil || !again.AlreadyInstalled || again.DepsInstalled {
		t.Fatalf("idempotent = %+v %v", again, err)
	}
	// Different node flags name a different request: --force required.
	other := InstallOptions{NodePackage: "agent-data", NodeVersion: "0.14.0", NoDeps: true}
	if _, err := InstallFromURL(t.Context(), profile, src, other, log); err == nil {
		t.Fatal("node mismatch accepted")
	} else if !strings.Contains(err.Error(), "different node dependencies") {
		t.Fatalf("wrong mismatch error: %v", err)
	}
}

func TestInstallNodeFlagsValidation(t *testing.T) {
	profile := t.TempDir()
	src := filepath.Join(t.TempDir(), "half-flagged")
	installFixture(t, src, "half-flagged", "")
	log := config.NewLogger(config.LevelWarn)
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{NodePackage: "agent-data", NoDeps: true}, log); err == nil {
		t.Fatal("half flags accepted")
	}
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{NodePackage: "not a package!", NodeVersion: "0.13.0", NoDeps: true}, log); err == nil {
		t.Fatal("bad package accepted")
	}
	// Flags conflicting with a manifest declaration fail closed.
	declared := filepath.Join(t.TempDir(), "declared")
	installFixture(t, declared, "declared", "node_package: shipped\nnode_version: 1.2.3\n")
	if _, err := InstallFromURL(t.Context(), profile, declared, InstallOptions{NodePackage: "other", NodeVersion: "9.9.9", NoDeps: true}, log); err == nil {
		t.Fatal("flag/manifest conflict accepted")
	} else if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("wrong conflict error: %v", err)
	}
	// Matching flags with a manifest declaration succeed.
	if _, err := InstallFromURL(t.Context(), profile, declared, InstallOptions{NodePackage: "shipped", NodeVersion: "1.2.3", NoDeps: true}, log); err != nil {
		t.Fatalf("matching flags: %v", err)
	}
}

func TestInstallLockfileFallbackRecorded(t *testing.T) {
	profile := t.TempDir()
	src := filepath.Join(t.TempDir(), "locked")
	installFixture(t, src, "locked", "")
	if err := os.WriteFile(filepath.Join(src, "package.json"), []byte("{\"name\":\"locked\",\"version\":\"1.0.0\"}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "package-lock.json"), []byte("{\"name\":\"locked\",\"version\":\"1.0.0\",\"lockfileVersion\":3}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	log := config.NewLogger(config.LevelWarn)
	if _, err := InstallFromURL(t.Context(), profile, src, InstallOptions{NoDeps: true}, log); err != nil {
		t.Fatalf("install: %v", err)
	}
	meta, err := ReadInstallMetadata(profile, "locked")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if !meta.NodeLockfile || meta.NodePackage != "" {
		t.Fatalf("meta = %+v", meta)
	}
	// A package.json without a lockfile records nothing.
	bare := filepath.Join(t.TempDir(), "bare")
	installFixture(t, bare, "bare", "")
	if err := os.WriteFile(filepath.Join(bare, "package.json"), []byte("{\"name\":\"bare\"}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallFromURL(t.Context(), profile, bare, InstallOptions{NoDeps: true}, log); err != nil {
		t.Fatalf("install: %v", err)
	}
	bareMeta, err := ReadInstallMetadata(profile, "bare")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if bareMeta.NodeLockfile || bareMeta.NodePackage != "" {
		t.Fatalf("bare meta = %+v", bareMeta)
	}
}

func TestResolveNodeSpec(t *testing.T) {
	manifest := Manifest{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1", NodePackage: "shipped", NodeVersion: "1.2.3"}
	meta := InstallMetadata{NodePackage: "agent-data", NodeVersion: "0.13.0"}
	if spec, ok := ResolveNodeSpec(manifest, meta); !ok || spec.Package != "shipped" {
		t.Fatalf("manifest should win: %+v %v", spec, ok)
	}
	plain := Manifest{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1"}
	if spec, ok := ResolveNodeSpec(plain, meta); !ok || spec.Package != "agent-data" {
		t.Fatalf("meta should carry: %+v %v", spec, ok)
	}
	locked := InstallMetadata{NodeLockfile: true}
	if spec, ok := ResolveNodeSpec(plain, locked); !ok || !spec.Lockfile {
		t.Fatalf("lockfile should carry: %+v %v", spec, ok)
	}
	if _, ok := ResolveNodeSpec(plain, InstallMetadata{}); ok {
		t.Fatal("empty spec reported present")
	}
	if got := EffectiveNodeBin(plain, meta, ""); got != "agent-data" {
		t.Fatalf("effective bin = %q", got)
	}
	if got := EffectiveNodeBin(manifest, meta, "custom"); got != "custom" {
		t.Fatalf("explicit bin = %q", got)
	}
	if got := EffectiveNodeBin(plain, InstallMetadata{}, ""); got != "" {
		t.Fatalf("lockfile-only bin = %q", got)
	}
}

func TestResolveNodeSidecar(t *testing.T) {
	dir := t.TempDir()
	manifest := &Manifest{Name: "demo", Source: "user", ManifestVersion: 2, ApiVersion: "1"}
	if spec, err := resolveNodeSidecar(manifest, "", "", dir); err != nil || spec.Present() {
		t.Fatalf("empty = %+v %v", spec, err)
	}
	if _, err := resolveNodeSidecar(manifest, "agent-data", "", dir); err == nil {
		t.Fatal("half flags accepted")
	}
	spec, err := resolveNodeSidecar(manifest, "agent-data", "0.13.0", dir)
	if err != nil || spec.Package != "agent-data" || spec.Version != "0.13.0" {
		t.Fatalf("flags = %+v %v", spec, err)
	}
}
