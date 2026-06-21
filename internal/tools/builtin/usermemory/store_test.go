package usermemory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
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

func TestStoreRememberAppendsWithoutDeletingExistingMemory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "users"), config.NewLogger(config.LevelError))
	store.SetSpeakerLineResolver(func(userID string) (string, error) {
		return "You are speaking with Test User.", nil
	})

	if err := store.Set("user1", "The user likes tea.", "tea evidence", "preferences"); err != nil {
		t.Fatalf("set first: %v", err)
	}
	if err := store.Set("user1", "The user likes purple.", "purple evidence", "preferences"); err != nil {
		t.Fatalf("set second: %v", err)
	}

	content, err := store.Read("user1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(content, "You are speaking with Test User.") || !strings.Contains(content, "The user likes tea.") || !strings.Contains(content, "The user likes purple.") {
		t.Fatalf("remember should preserve profile and existing entries, got:\n%s", content)
	}

	var entryCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM user_memory_entries WHERE canonical_user_id = ?`, "user1").Scan(&entryCount); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if entryCount != 2 {
		t.Fatalf("expected 2 appended entries, got %d", entryCount)
	}
}

func TestStoreRecallIsScopedPerUser(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "users"), config.NewLogger(config.LevelError))
	if err := store.Set("user1", "The user likes tea.", "tea evidence", "preferences"); err != nil {
		t.Fatalf("set user1: %v", err)
	}
	if err := store.Set("user2", "The user likes coffee.", "coffee evidence", "preferences"); err != nil {
		t.Fatalf("set user2: %v", err)
	}

	content, err := store.Read("user1")
	if err != nil {
		t.Fatalf("read user1: %v", err)
	}
	if !strings.Contains(content, "The user likes tea.") || strings.Contains(content, "The user likes coffee.") {
		t.Fatalf("user1 recall leaked user2 memory:\n%s", content)
	}

	category, err := store.ReadCategory("user1", "preferences")
	if err != nil {
		t.Fatalf("read user1 category: %v", err)
	}
	if !strings.Contains(category, "The user likes tea.") || strings.Contains(category, "The user likes coffee.") {
		t.Fatalf("user1 category recall leaked user2 memory:\n%s", category)
	}
}

func TestStoreForgetIsScopedPerUser(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "users"), config.NewLogger(config.LevelError))
	statement := "The user likes tea."
	if err := store.Set("user1", statement, "user1 evidence", "preferences"); err != nil {
		t.Fatalf("set user1: %v", err)
	}
	if err := store.Set("user2", statement, "user2 evidence", "preferences"); err != nil {
		t.Fatalf("set user2: %v", err)
	}

	if err := store.Delete("user1", statement); err != nil {
		t.Fatalf("delete user1: %v", err)
	}
	user1, err := store.Read("user1")
	if err != nil {
		t.Fatalf("read user1: %v", err)
	}
	if strings.Contains(user1, statement) {
		t.Fatalf("expected user1 memory deleted, got:\n%s", user1)
	}
	user2, err := store.Read("user2")
	if err != nil {
		t.Fatalf("read user2: %v", err)
	}
	if !strings.Contains(user2, statement) || !strings.Contains(user2, "user2 evidence") {
		t.Fatalf("user1 forget deleted or changed user2 memory:\n%s", user2)
	}

	if err := store.DeleteAll("user1"); err != nil {
		t.Fatalf("delete all user1: %v", err)
	}
	user2, err = store.Read("user2")
	if err != nil {
		t.Fatalf("read user2 after delete all: %v", err)
	}
	if !strings.Contains(user2, statement) || !strings.Contains(user2, "user2 evidence") {
		t.Fatalf("user1 delete all deleted or changed user2 memory:\n%s", user2)
	}
}

func TestMemoryToolHandlersUseContextUserScope(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "users"), config.NewLogger(config.LevelError))
	log := config.NewLogger(config.LevelError)
	remember := NewRememberHandler(store, log)
	recall := NewRecallHandler(store, nil, "", log)
	forget := NewForgetHandler(store, log)

	user1Ctx := requestctx.WithSenderID(context.Background(), "user1")
	user2Ctx := requestctx.WithSenderID(context.Background(), "user2")
	if _, err := remember(user1Ctx, map[string]interface{}{
		"statement": "The user likes tea.",
		"evidence":  "user1 evidence",
		"category":  "preferences",
		"user_id":   "user2",
	}); err != nil {
		t.Fatalf("remember user1: %v", err)
	}
	if _, err := remember(user2Ctx, map[string]interface{}{
		"statement": "The user likes coffee.",
		"evidence":  "user2 evidence",
		"category":  "preferences",
		"user_id":   "user1",
	}); err != nil {
		t.Fatalf("remember user2: %v", err)
	}

	user1Memory, err := recall(user1Ctx, map[string]interface{}{"user_id": "user2"})
	if err != nil {
		t.Fatalf("recall user1: %v", err)
	}
	if !strings.Contains(user1Memory, "The user likes tea.") || strings.Contains(user1Memory, "The user likes coffee.") {
		t.Fatalf("recall did not stay scoped to context user:\n%s", user1Memory)
	}

	if _, err := forget(user1Ctx, map[string]interface{}{"statement": "all", "user_id": "user2"}); err != nil {
		t.Fatalf("forget user1: %v", err)
	}
	user2Memory, err := recall(user2Ctx, map[string]interface{}{})
	if err != nil {
		t.Fatalf("recall user2: %v", err)
	}
	if !strings.Contains(user2Memory, "The user likes coffee.") || strings.Contains(user2Memory, "The user likes tea.") {
		t.Fatalf("forget leaked across context users:\n%s", user2Memory)
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

func TestStoreMigratesLegacyMarkdownWithoutDeletingBackup(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "users")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, "user1.md")
	legacyContent := "# User Memory\n\nYou are speaking with Legacy User.\n\n## Preferences\n\nThe user likes green tea.\n\n- Evidence: legacy evidence.\n"
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	store, err := NewSQLiteStore(filepath.Join(dir, "oswald.db"), legacyDir, nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	defer store.Close() // nolint:errcheck
	if err := store.MigrateLegacyMarkdown(); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}

	content, err := store.Read("user1")
	if err != nil {
		t.Fatalf("read migrated: %v", err)
	}
	if !strings.Contains(content, "Legacy User") || !strings.Contains(content, "The user likes green tea.") {
		t.Fatalf("unexpected migrated content:\n%s", content)
	}
	backup, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != legacyContent {
		t.Fatalf("legacy backup changed:\n%s", string(backup))
	}
}

func TestStoreSemanticSearchUsesSQLiteVec(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "oswald.db"), "", fakeMemoryEmbedder{}, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	defer store.Close() // nolint:errcheck

	if err := store.SetWithContext(context.Background(), "user1", "The user likes tea.", "tea evidence", "preferences"); err != nil {
		t.Fatalf("set tea: %v", err)
	}
	if err := store.SetWithContext(context.Background(), "user1", "The user likes coffee.", "coffee evidence", "preferences"); err != nil {
		t.Fatalf("set coffee: %v", err)
	}

	content, err := store.Search(context.Background(), "user1", "preferences", "tea", 1)
	if err != nil {
		t.Fatalf("semantic search: %v", err)
	}
	if !strings.Contains(content, "The user likes tea.") || strings.Contains(content, "The user likes coffee.") {
		t.Fatalf("unexpected semantic results:\n%s", content)
	}
}

func TestStoreSemanticSearchIsScopedPerUser(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "oswald.db"), "", fakeMemoryEmbedder{}, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	defer store.Close() // nolint:errcheck

	if err := store.SetWithContext(context.Background(), "user1", "The user likes tea.", "tea evidence", "preferences"); err != nil {
		t.Fatalf("set user1: %v", err)
	}
	if err := store.SetWithContext(context.Background(), "user2", "The user likes coffee.", "coffee evidence", "preferences"); err != nil {
		t.Fatalf("set user2: %v", err)
	}

	content, err := store.Search(context.Background(), "user1", "", "coffee", 10)
	if err != nil {
		t.Fatalf("semantic search: %v", err)
	}
	if strings.Contains(content, "The user likes coffee.") || strings.Contains(content, "coffee evidence") {
		t.Fatalf("semantic search leaked user2 memory:\n%s", content)
	}
}

func TestStoreBackfillEmbeddingsPopulatesVectorsForMigratedMemory(t *testing.T) {
	dir := t.TempDir()
	legacyDir := filepath.Join(dir, "users")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	legacyContent := "# User Memory\n\n## Preferences\n\nThe user likes tea.\n\n- Evidence: legacy evidence.\n"
	if err := os.WriteFile(filepath.Join(legacyDir, "user1.md"), []byte(legacyContent), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	store, err := NewSQLiteStore(filepath.Join(dir, "oswald.db"), legacyDir, fakeMemoryEmbedder{}, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	defer store.Close() // nolint:errcheck
	if err := store.MigrateLegacyMarkdown(); err != nil {
		t.Fatalf("migrate legacy: %v", err)
	}
	if err := store.BackfillEmbeddings(context.Background()); err != nil {
		t.Fatalf("backfill embeddings: %v", err)
	}

	var configTableCount int
	err = store.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'user_memory_embedding_config'`).Scan(&configTableCount)
	if err != nil {
		t.Fatalf("inspect embedding config table: %v", err)
	}
	if configTableCount != 0 {
		t.Fatalf("expected no user_memory_embedding_config table, got %d", configTableCount)
	}
	var vectorTableCount int
	err = store.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'user_memory_vectors'`).Scan(&vectorTableCount)
	if err != nil {
		t.Fatalf("inspect vector table: %v", err)
	}
	if vectorTableCount != 1 {
		t.Fatalf("expected user_memory_vectors table, got %d", vectorTableCount)
	}

	content, err := store.Search(context.Background(), "user1", "preferences", "tea", 1)
	if err != nil {
		t.Fatalf("semantic search: %v", err)
	}
	if !strings.Contains(content, "The user likes tea.") {
		t.Fatalf("expected migrated memory in semantic search, got:\n%s", content)
	}
}

type fakeMemoryEmbedder struct{}

func (fakeMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	input := strings.ToLower(req.Input)
	vec := []float64{0, 1}
	if strings.Contains(input, "tea") {
		vec = []float64{1, 0}
	}
	return &llm.EmbedResponse{Model: req.Model, Embeddings: [][]float64{vec}}, nil
}
