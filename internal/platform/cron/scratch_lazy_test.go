package cron

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureScratchDirCreatesLazily(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	ensureScratchDir(root, scratch)
	info, err := os.Stat(scratch)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("scratch not created private: %v %v", info, err)
	}
	// Idempotent and repairs loose permissions.
	if err := os.Chmod(scratch, 0755); err != nil {
		t.Fatal(err)
	}
	ensureScratchDir(root, scratch)
	if info, err := os.Stat(scratch); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("scratch not repaired: %v %v", info, err)
	}
	// Escapes and empty inputs are no-ops.
	ensureScratchDir(root, "/tmp/oswald-evil-scratch")
	if _, err := os.Stat("/tmp/oswald-evil-scratch"); !os.IsNotExist(err) {
		t.Fatal("escape path was created")
	}
	ensureScratchDir("", scratch)
	ensureScratchDir(root, "")
}

func TestRunScriptFileCreatesScratch(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "job.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, "cache", "scratch")
	r := &Runner{Root: root, Scratch: scratch, Resolve: func(raw string) (string, error) { return raw, nil }}
	out, err := runScriptFile(context.Background(), r, script, "", 30)
	if err != nil || out != "hi" {
		t.Fatalf("script failed: %q %v", out, err)
	}
	if info, err := os.Stat(scratch); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("scratch not created by script run: %v %v", info, err)
	}
}
