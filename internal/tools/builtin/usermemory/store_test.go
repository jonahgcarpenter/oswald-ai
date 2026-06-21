package usermemory

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestStoreSetReadCategoryAndDeleteAll(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "users"), config.NewLogger(config.LevelError))
	store.SetSpeakerLineResolver(func(userID string) (string, error) {
		return "You are speaking with Test User.", nil
	})

	if err := store.Set("user1", "The user likes tea.", "They said so", "preferences"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := store.Set("user1", "The user likes coffee.", "They corrected it", "preferences"); err != nil {
		t.Fatalf("set second: %v", err)
	}

	content, err := store.ReadCategory("user1", "preferences")
	if err != nil {
		t.Fatalf("read category: %v", err)
	}
	if !strings.Contains(content, "The user likes tea.") || !strings.Contains(content, "The user likes coffee.") || !strings.Contains(content, "They corrected it") {
		t.Fatalf("unexpected category content:\n%s", content)
	}

	intro, err := store.ReadIntro("user1")
	if err != nil {
		t.Fatalf("read intro: %v", err)
	}
	if intro != "You are speaking with Test User." {
		t.Fatalf("unexpected intro %q", intro)
	}

	if err := store.DeleteAll("user1"); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	content, err = store.Read("user1")
	if err != nil {
		t.Fatalf("read after delete all: %v", err)
	}
	if content != "" {
		t.Fatalf("expected memory removed, got:\n%s", content)
	}
}

func TestStoreMigratesFlatFileAndDefaultsCategory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "users"), config.NewLogger(config.LevelError))
	if err := store.WriteFull("user1", "Old flat fact.\n- Evidence: old"); err != nil {
		t.Fatalf("write full: %v", err)
	}
	if err := store.Set("user1", "New default fact.", "new", ""); err != nil {
		t.Fatalf("set default: %v", err)
	}

	content, err := store.ReadCategory("user1", "notes")
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	if !strings.Contains(content, "Old flat fact.") || !strings.Contains(content, "New default fact.") {
		t.Fatalf("expected migrated and new notes, got:\n%s", content)
	}
}

func TestStoreMergeUsersDeduplicatesStatements(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "users"), config.NewLogger(config.LevelError))
	if err := store.WriteFull("winner", "# User Memory\n\n## Preferences\n\nThe user likes tea.\n- Evidence: winner evidence.\n"); err != nil {
		t.Fatalf("write winner: %v", err)
	}
	if err := store.WriteFull("loser", "# User Memory\n\n## Identity\n\nThe user lives in Berlin.\n- Evidence: loser evidence.\n\n## Preferences\n\nThe user likes tea.\n- Evidence: loser evidence.\n"); err != nil {
		t.Fatalf("write loser: %v", err)
	}

	if err := store.MergeUsers("winner", "loser"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	winner, err := store.Read("winner")
	if err != nil {
		t.Fatalf("read winner: %v", err)
	}
	if strings.Count(winner, "The user likes tea.") != 1 || !strings.Contains(winner, "The user lives in Berlin.") {
		t.Fatalf("unexpected merged content:\n%s", winner)
	}
	loser, err := store.Read("loser")
	if err != nil {
		t.Fatalf("read loser: %v", err)
	}
	if loser != "" {
		t.Fatalf("expected loser memory removed, got %q", loser)
	}
}
