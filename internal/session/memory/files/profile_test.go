package files

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileMemoryUsesDirectRootAndRejectsOtherOwners(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store := NewProfileStore(root, "alice")
	ctx := context.Background()
	if _, err := store.Apply(ctx, "alice", "memory", []Operation{{Action: "add", Content: "synthetic note"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "memories", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Read(ctx, "bob"); err == nil {
		t.Fatal("cross-profile memory read accepted")
	}
	if _, err := store.Apply(ctx, "bob", "memory", []Operation{{Action: "add", Content: "other note"}}); err == nil {
		t.Fatal("cross-profile memory write accepted")
	}
	missing := filepath.Join(root, "missing")
	missingStore := NewProfileStore(missing, "alice")
	if _, err := missingStore.Apply(ctx, "alice", "memory", []Operation{{Action: "add", Content: "note"}}); err == nil {
		t.Fatal("missing profile created")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing operator directory was created")
	}
}
