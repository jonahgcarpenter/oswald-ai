package setup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func seedRoot(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, config.OswaldHomeDir)
}

func TestSetupSeedsDefaultsWithPrivatePermissions(t *testing.T) {
	root := seedRoot(t)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("setup = %d, stderr=%s", code, stderr.String())
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{".", 0700},
		{"profiles", 0700},
		{"config.yaml", 0600},
		{".env", 0600},
		{"SOUL.md", 0600},
	} {
		info, err := os.Stat(filepath.Join(root, tc.name))
		if err != nil {
			t.Fatalf("stat %q: %v", tc.name, err)
		}
		if info.Mode().Perm() != tc.mode {
			t.Fatalf("%q mode = %v, want %v", tc.name, info.Mode().Perm(), tc.mode)
		}
	}
	for name, want := range map[string]string{"config.yaml": defaultConfig, ".env": defaultEnv, "SOUL.md": defaultSoul} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s content mismatch: %v", name, err)
		}
	}
	// The seeded root must load without credentials because disabled platforms
	// are pruned before interpolation.
	if _, err := config.LoadProfiles(root); err != nil {
		t.Fatalf("seeded configuration did not load: %v", err)
	}
}

func TestSetupIsIdempotentAndNeverOverwrites(t *testing.T) {
	root := seedRoot(t)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("first setup = %d", code)
	}
	edited := "providers: {}\n"
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := Run(context.Background(), root, nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("second setup = %d", code)
	}
	if !strings.Contains(stdout.String(), "kept") {
		t.Fatalf("second setup did not report kept files: %q", stdout.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil || string(data) != edited {
		t.Fatal("setup overwrote operator content")
	}
}

func TestSetupWarnsOnInsecureExistingFile(t *testing.T) {
	root := seedRoot(t)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), root, nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("seed = %d", code)
	}
	if err := os.Chmod(filepath.Join(root, ".env"), 0644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), root, nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("re-seed = %d", code)
	}
	if !strings.Contains(stderr.String(), "chmod 600") {
		t.Fatalf("missing insecure-permission warning: %q", stderr.String())
	}
}

func TestSetupRejectsSymlinkedRoot(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), link, nil, &stdout, &stderr); code != exitError {
		t.Fatalf("symlinked root = %d, want error", code)
	}
	if _, err := os.Stat(filepath.Join(target, "config.yaml")); !os.IsNotExist(err) {
		t.Fatal("setup wrote through a symlinked root")
	}
}
