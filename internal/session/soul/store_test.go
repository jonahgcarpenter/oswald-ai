package soul

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileSoulIsReadFreshWithoutCopyingTemplate(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "SOUL.md")
	if err := os.WriteFile(template, []byte("default"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewProfileStore(profile, "one", template)
	if got, err := store.Read(context.Background(), "one"); err != nil || got != "default" {
		t.Fatalf("fallback: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(profile, "SOUL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("template copied")
	}
	for _, policy := range []string{"personal", "edited"} {
		if err := os.WriteFile(filepath.Join(profile, "SOUL.md"), []byte(policy), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Read(context.Background(), "one"); err != nil || got != policy {
			t.Fatalf("fresh policy: %q %v", got, err)
		}
	}
	if _, err := store.Read(context.Background(), "two"); err == nil {
		t.Fatal("other owner accepted")
	}
}

func TestProfileSoulRejectsUnsafeFilesMissingRootAndCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "SOUL.md")
	store := NewProfileStore(root, "one", path)
	if _, err := store.Read(context.Background(), "one"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing policy accepted")
	}
	if err := os.Symlink(filepath.Join(root, "outside"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "one"); err == nil {
		t.Fatal("symlink accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Read(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	missing := filepath.Join(root, "absent")
	if _, err := NewProfileStore(missing, "one", path).Read(context.Background(), "one"); err == nil {
		t.Fatal("missing profile accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("profile created")
	}
}
