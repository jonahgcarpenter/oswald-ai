package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWritePrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := AtomicWritePrivateFile(path, []byte("a: 1\n")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("not owner-only: %v %v", info, err)
	}
	if err := AtomicWritePrivateFile(path, []byte("a: 2\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "a: 2\n" {
		t.Fatalf("rewrite failed: %q %v", data, err)
	}
	if err := SaveLastKnownGood(path, []byte("a: 1\n")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLastKnownGood(path)
	if err != nil || string(got) != "a: 1\n" {
		t.Fatalf("last-known-good failed: %q %v", got, err)
	}
	if _, err := ReadLastKnownGood(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing last-known-good accepted")
	}
}
