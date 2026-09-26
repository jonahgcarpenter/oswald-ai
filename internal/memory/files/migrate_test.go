package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateLegacyFilesAndResume(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "one")
	if err := os.Mkdir(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"USER.md", "MEMORY.md"} {
		if err := os.WriteFile(filepath.Join(oldDir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldDir, name+".lock"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := NewStore(root)
	if moved, err := store.Migrate(context.Background()); err != nil || moved != 2 {
		t.Fatalf("migrate: moved=%d err=%v", moved, err)
	}
	user, memory, err := NewStore(root).Read(context.Background(), "one")
	if err != nil || user != "USER.md" || memory != "MEMORY.md" {
		t.Fatalf("reopen: user=%q memory=%q err=%v", user, memory, err)
	}
	for _, name := range []string{"USER.md", "MEMORY.md"} {
		if _, err := os.Stat(filepath.Join(oldDir, name)); !os.IsNotExist(err) {
			t.Fatalf("legacy file remains: %s %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(oldDir, name+".lock")); err != nil {
			t.Fatalf("legacy lock removed: %s %v", name, err)
		}
	}
	if moved, err := store.Migrate(context.Background()); err != nil || moved != 0 {
		t.Fatalf("idempotent migration: moved=%d err=%v", moved, err)
	}
	if _, err := store.Apply(context.Background(), "one", "user", []Operation{{Action: "add", Content: "updated"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(oldDir, "memories", "USER.md.lock")); err != nil {
		t.Fatalf("new lock removed: %v", err)
	}
}

func TestMigrateRejectsConflictAndUnsafeLegacyFile(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "one")
	if err := os.Mkdir(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"USER.md", "MEMORY.md"} {
		if err := os.WriteFile(filepath.Join(oldDir, name), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store := NewStore(root)
	if _, err := store.Apply(context.Background(), "one", "memory", []Operation{{Action: "add", Content: "new"}}); err != nil {
		t.Fatal(err)
	}
	if moved, err := store.Migrate(context.Background()); err == nil || moved != 0 {
		t.Fatalf("conflict: moved=%d err=%v", moved, err)
	}
	if data, err := os.ReadFile(filepath.Join(oldDir, "USER.md")); err != nil || string(data) != "original" {
		t.Fatalf("moved before detecting conflict: %q %v", data, err)
	}
	if err := os.Remove(filepath.Join(oldDir, "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(oldDir, "USER.md"), 0644); err != nil {
		t.Fatal(err)
	}
	if moved, err := store.Migrate(context.Background()); err == nil || moved != 0 {
		t.Fatalf("unsafe file: moved=%d err=%v", moved, err)
	}
	if err := os.Chmod(filepath.Join(oldDir, "USER.md"), 0600); err != nil {
		t.Fatal(err)
	}
	if moved, err := store.Migrate(context.Background()); err != nil || moved != 1 {
		t.Fatalf("resume: moved=%d err=%v", moved, err)
	}
}
