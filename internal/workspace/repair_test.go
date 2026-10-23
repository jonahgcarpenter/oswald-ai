package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepairCacheRepairsWithoutCreating(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"cache", "cache/scratch"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	RepairCache(root, "test", nil)
	for _, dir := range []string{"cache", "cache/scratch"} {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("%s not repaired: %v %v", dir, info, err)
		}
	}
	for _, dir := range []string{"cache/images", "cache/documents", "cache/processes", "cache/browser"} {
		if _, err := os.Stat(filepath.Join(root, dir)); !os.IsNotExist(err) {
			t.Fatalf("%s created eagerly", dir)
		}
	}
}

func TestRepairCacheSkipsMissingAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	RepairCache(root, "test", nil)
	if _, err := os.Stat(filepath.Join(root, "cache")); !os.IsNotExist(err) {
		t.Fatal("missing cache was created")
	}
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "target")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "cache", "documents")); err != nil {
		t.Fatal(err)
	}
	RepairCache(root, "test", nil)
	info, err := os.Lstat(filepath.Join(root, "cache", "documents"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was not left alone")
	}
}
