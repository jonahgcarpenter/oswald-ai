package soul

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileSoulFallsBackWithoutCreatingOperatorFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "SOUL.md")
	if err := os.WriteFile(template, []byte("default policy"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profiles", "alice")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	store := NewProfileStore(profile, "alice", template)
	if text, err := store.Read(context.Background(), "alice"); err != nil || text != "default policy" {
		t.Fatal("fallback failed", err)
	}
	if _, err := os.Stat(filepath.Join(profile, "SOUL.md")); !os.IsNotExist(err) {
		t.Fatal("operator soul was created")
	}
	if err := os.WriteFile(filepath.Join(profile, "SOUL.md"), []byte("profile policy"), 0600); err != nil {
		t.Fatal(err)
	}
	if text, err := store.Read(context.Background(), "alice"); err != nil || text != "profile policy" {
		t.Fatal("profile soul not read", err)
	}
	if _, err := store.Read(context.Background(), "bob"); err == nil {
		t.Fatal("cross-profile soul read accepted")
	}
	defaultStore := NewProfileStore(root, "default", template)
	if text, err := defaultStore.Read(context.Background(), "default"); err != nil || text != "default policy" {
		t.Fatal("default profile failed", err)
	}
}
