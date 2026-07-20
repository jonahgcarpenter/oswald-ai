package soul

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreReadsOperatorManagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "soul.md")
	store := NewStore(path)
	if got, err := store.Read(); err != nil || got != "" {
		t.Fatalf("missing read got %q err %v", got, err)
	}
	if err := os.WriteFile(path, []byte("alpha\nbeta"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := store.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "alpha\nbeta" {
		t.Fatalf("unexpected content %q", got)
	}
}
