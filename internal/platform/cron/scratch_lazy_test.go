package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureScratchDirCreatesLazily(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	ensureScratchDir(root, scratch, "")
	info, err := os.Stat(scratch)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("scratch not created private: %v %v", info, err)
	}
	// Idempotent and repairs loose permissions.
	if err := os.Chmod(scratch, 0755); err != nil {
		t.Fatal(err)
	}
	ensureScratchDir(root, scratch, "")
	if info, err := os.Stat(scratch); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("scratch not repaired: %v %v", info, err)
	}
	// Escapes and empty inputs are no-ops.
	ensureScratchDir(root, "/tmp/oswald-evil-scratch", "")
	if _, err := os.Stat("/tmp/oswald-evil-scratch"); !os.IsNotExist(err) {
		t.Fatal("escape path was created")
	}
	ensureScratchDir("", scratch, "")
	ensureScratchDir(root, "", "")
}

func TestRunScriptFileCreatesScratch(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "job.sh"), []byte("#!/bin/sh\necho hi\n"), 0600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	r := &Runner{Root: root, ScriptsDir: scripts, Scratch: scratch}
	out, err := runScriptFile(context.Background(), r, "job.sh", 30)
	if err != nil || out != "hi" {
		t.Fatalf("script failed: %q %v", out, err)
	}
	if info, err := os.Stat(scratch); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("scratch not created by script run: %v %v", info, err)
	}
}

// TestRunScriptFileAlignsEnvWithTerminal keeps cron scripts on the same
// sandbox contract as terminal so a tested command behaves identically.
func TestRunScriptFileAlignsEnvWithTerminal(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0700); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nprintf '%s|%s|%s|%s' \"$HOME\" \"$OSWALD_HOME\" \"$XDG_CACHE_HOME\" \"$PATH\"\n"
	if err := os.WriteFile(filepath.Join(scripts, "env.sh"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	r := &Runner{Root: root, ScriptsDir: scripts, Scratch: scratch, Profile: "alice"}
	out, err := runScriptFile(context.Background(), r, "env.sh", 30)
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	prefix := root + "|" + root + "|" + filepath.Join(root, "cache", "xdg") + "|"
	if !strings.HasPrefix(out, prefix) {
		t.Fatalf("cron env = %q, want prefix %q", out, prefix)
	}
	if !strings.Contains(out, "/opt/oswald") {
		t.Fatalf("cron PATH missing /opt/oswald: %q", out)
	}
}
