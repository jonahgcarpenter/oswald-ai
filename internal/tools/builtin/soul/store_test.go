package soul

import (
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestPatchContentOperationsPreserveTrailingNewline(t *testing.T) {
	current := "alpha\nbeta\ngamma\n"

	got, err := patchContent(current, "replace", "beta", "BETA", "", "")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("unexpected replace result %q", got)
	}

	got, err = patchContent(current, "add", "", "insert", "before", "beta")
	if err != nil {
		t.Fatalf("add before: %v", err)
	}
	if got != "alpha\ninsert\nbeta\ngamma\n" {
		t.Fatalf("unexpected add result %q", got)
	}

	got, err = patchContent(current, "remove", "beta", "", "", "")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got != "alpha\ngamma\n" {
		t.Fatalf("unexpected remove result %q", got)
	}
}

func TestPatchContentRejectsAmbiguousOrMissingLines(t *testing.T) {
	if _, err := patchContent("x\nx", "replace", "x", "y", "", ""); err == nil {
		t.Fatal("expected duplicate target error")
	}
	if _, err := patchContent("x", "remove", "missing", "", "", ""); err == nil {
		t.Fatal("expected missing target error")
	}
	if _, err := patchContent("x", "add", "", "y", "middle", "x"); err == nil {
		t.Fatal("expected invalid position error")
	}
}

func TestStoreReadWritePatch(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "soul.md"), config.NewLogger(config.LevelError))
	if got, err := store.Read(); err != nil || got != "" {
		t.Fatalf("missing read got %q err %v", got, err)
	}
	if err := store.Write("alpha\nbeta"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := store.Patch("add", "", "gamma", "end", ""); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got, err := store.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "alpha\nbeta\ngamma" {
		t.Fatalf("unexpected content %q", got)
	}
}
