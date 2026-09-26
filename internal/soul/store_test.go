package soul

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreCopiesOnceAndReadsEachUserFresh(t *testing.T) {
	root := t.TempDir()
	template := filepath.Join(root, "SOUL.md")
	if err := os.WriteFile(template, []byte("default"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(template)
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := store.Read(context.Background(), "one")
			if err == nil && got != "default" {
				results <- os.ErrInvalid
				return
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	userSoul := filepath.Join(root, "one", "SOUL.md")
	if info, err := os.Stat(userSoul); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private soul mode: %v %v", info, err)
	}
	if err := os.WriteFile(userSoul, []byte("personal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(template, []byte("new default"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Read(context.Background(), "one"); err != nil || got != "personal" {
		t.Fatalf("operator edit overwritten: %q %v", got, err)
	}
	if got, err := store.Read(context.Background(), "two"); err != nil || got != "new default" {
		t.Fatalf("new user's template: %q %v", got, err)
	}
	if err := store.Delete(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(userSoul); !os.IsNotExist(err) {
		t.Fatalf("soul was not deleted: %v", err)
	}
}

func TestStoreRejectsUnsafePathsAndMissingTemplate(t *testing.T) {
	root := t.TempDir()
	store := NewStore(filepath.Join(root, "SOUL.md"))
	if _, err := store.Read(context.Background(), "one"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing template: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "one", "SOUL.md")); !os.IsNotExist(err) {
		t.Fatalf("missing template created private soul: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "one", "SOUL.md"), []byte("existing private soul"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Read(context.Background(), "one"); err != nil || got != "existing private soul" {
		t.Fatalf("existing soul should not require template: %q %v", got, err)
	}
	if err := os.Remove(filepath.Join(root, "one", "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("default"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "../outside"); err == nil {
		t.Fatal("accepted unsafe ID")
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "one", "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "one"); err == nil {
		t.Fatal("followed user soul symlink")
	}
	if err := store.Delete(context.Background(), "one"); err == nil {
		t.Fatal("deleted symlinked soul")
	}
}

func TestMigrateTemplateRefusesDifferentDestination(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "legacy.md")
	newPath := filepath.Join(root, "SOUL.md")
	if err := os.WriteFile(old, []byte("operator edit"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(newPath)
	if err := store.MigrateTemplate(old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("legacy remains: %v", err)
	}
	if err := os.WriteFile(old, []byte("other edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateTemplate(old); err == nil {
		t.Fatal("silently overwrote new template")
	}
	if data, err := os.ReadFile(newPath); err != nil || string(data) != "operator edit" {
		t.Fatalf("new template changed: %q %v", data, err)
	}
	if err := os.WriteFile(old, []byte("operator edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateTemplate(old); err != nil {
		t.Fatalf("identical templates: %v", err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identical legacy template remains: %v", err)
	}
}
