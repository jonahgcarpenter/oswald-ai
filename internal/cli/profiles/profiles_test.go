package profiles

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// profileRoot builds a minimal valid global root plus the API profile required
// by the enabled API platform, so created profiles can be validated by the real
// configuration loader.
func profileRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("default soul policy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(globalConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "profiles", "api"), 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

const globalConfig = `providers:
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
`

func TestCreateProvisionsPrivateProfile(t *testing.T) {
	root := profileRoot(t)
	var stdout, stderr bytes.Buffer
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("create = %d, stderr=%s", code, stderr.String())
	}
	dir := filepath.Join(root, "profiles", "alice")
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"", 0700},
		{".env", 0600},
		{"config.yaml", 0600},
		{"SOUL.md", 0600},
		{"state.db", 0600},
	} {
		info, err := os.Stat(filepath.Join(dir, tc.name))
		if err != nil {
			t.Fatalf("stat %q: %v", tc.name, err)
		}
		if info.Mode().Perm() != tc.mode {
			t.Fatalf("%q mode = %v, want %v", tc.name, info.Mode().Perm(), tc.mode)
		}
	}
	soul, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil || string(soul) != "default soul policy" {
		t.Fatalf("soul copy = %q err=%v", soul, err)
	}
	// A route makes the loader resolve the new profile, proving its scaffolded
	// config.yaml is valid and its directory is private.
	routed := globalConfig + "gateway:\n  multiplex_profiles: true\n  profile_routes:\n    - platform: discord\n      user_id: \"123\"\n      profile: alice\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(routed), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadProfiles(root)
	if err != nil {
		t.Fatalf("load after create: %v", err)
	}
	if cfg.Profiles["alice"] == nil {
		t.Fatal("created profile was not discovered by the loader")
	}
}

func TestCreateAcceptsExistingPublicProfilesContainer(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("soul"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(globalConfig), 0600); err != nil {
		t.Fatal(err)
	}
	// A group/other-readable profiles container is valid at runtime because each
	// profile directory is private; create must not reject it.
	if err := os.MkdirAll(filepath.Join(root, "profiles", "api"), 0755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("create with 0755 profiles container = %d: %s", code, stderr.String())
	}
	info, err := os.Stat(filepath.Join(root, "profiles", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("profile dir mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestCreateRejectsInvalidNamesAndExistingProfile(t *testing.T) {
	root := profileRoot(t)
	var stdout, stderr bytes.Buffer
	for _, name := range []string{"default", "../escape", "Upper", "", "has space"} {
		if code := create(context.Background(), root, []string{name}, &stdout, &stderr); code != exitUsage {
			t.Fatalf("create(%q) = %d, want usage error", name, code)
		}
	}
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("initial create = %d", code)
	}
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitError {
		t.Fatalf("duplicate create = %d, want error", code)
	}
}

func TestCreateRequiresDefaultSoul(t *testing.T) {
	root := profileRoot(t)
	if err := os.Remove(filepath.Join(root, "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitError {
		t.Fatalf("create without default soul = %d, want error", code)
	}
	if _, err := os.Stat(filepath.Join(root, "profiles", "alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed create left a partial profile directory")
	}
}

func TestCreateRollsBackOnDatabaseFailure(t *testing.T) {
	root := profileRoot(t)
	original := openProfileState
	openProfileState = func(context.Context, string) error { return errors.New("synthetic database failure") }
	defer func() { openProfileState = original }()

	var stdout, stderr bytes.Buffer
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitError {
		t.Fatalf("create with failing database = %d, want error", code)
	}
	if _, err := os.Stat(filepath.Join(root, "profiles", "alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial profile directory was not rolled back")
	}
}

func TestDeleteRemovesProfileAfterConfirmation(t *testing.T) {
	root := profileRoot(t)
	dir := filepath.Join(root, "profiles", "alice")
	var stdout, stderr bytes.Buffer
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("create = %d", code)
	}
	if code := deleteProfile(context.Background(), root, []string{"alice"}, strings.NewReader("y\n"), &stdout, &stderr); code != exitOK {
		t.Fatalf("delete = %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("profile was not deleted")
	}
}

func TestDeleteAbortsWithoutConfirmation(t *testing.T) {
	root := profileRoot(t)
	dir := filepath.Join(root, "profiles", "alice")
	var stdout, stderr bytes.Buffer
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("create = %d", code)
	}
	for _, answer := range []string{"n\n", "\n", "", "maybe\n"} {
		if code := deleteProfile(context.Background(), root, []string{"alice"}, strings.NewReader(answer), &stdout, &stderr); code != exitError {
			t.Fatalf("delete with %q = %d, want error", answer, code)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("profile deleted despite answer %q", answer)
		}
	}
}

func TestDeleteRejectsInvalidAndMissing(t *testing.T) {
	root := profileRoot(t)
	var stdout, stderr bytes.Buffer
	if code := deleteProfile(context.Background(), root, []string{"default"}, strings.NewReader("y\n"), &stdout, &stderr); code != exitUsage {
		t.Fatalf("delete default = %d, want usage error", code)
	}
	if code := deleteProfile(context.Background(), root, []string{"ghost"}, strings.NewReader("y\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("delete missing = %d, want error", code)
	}
}

func TestDeleteWarnsAboutDanglingRoute(t *testing.T) {
	root := profileRoot(t)
	var stdout, stderr bytes.Buffer
	if code := create(context.Background(), root, []string{"alice"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("create = %d", code)
	}
	routed := globalConfig + "gateway:\n  multiplex_profiles: true\n  profile_routes:\n    - name: alice-discord\n      platform: discord\n      user_id: \"123\"\n      profile: alice\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(routed), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := deleteProfile(context.Background(), root, []string{"alice"}, strings.NewReader("n\n"), &stdout, &stderr); code != exitError {
		t.Fatalf("delete abort = %d, want error", code)
	}
	if !strings.Contains(stdout.String(), "alice-discord") || !strings.Contains(stdout.String(), "startup fails") {
		t.Fatalf("dangling route warning missing: %q", stdout.String())
	}
}
