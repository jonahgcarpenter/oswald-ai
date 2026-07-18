package usermemory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

type fakeMemoryEmbedder struct{}

func (fakeMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

type countingMemoryEmbedder struct {
	inputs []string
}

func (f *countingMemoryEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.inputs = append(f.inputs, req.Input)
	if strings.Contains(req.Input, "purple") {
		return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{0, 1}}}, nil
}

func TestStoreSaveSearchAndForgetMemory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.SyncSpeakerIntro("usr_test", "You are speaking with Test User."); err != nil {
		t.Fatal(err)
	}
	entry, err := store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Scope != ScopeLongTerm || entry.Category != "durable_preferences" {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	entries, err := store.Search(context.Background(), "usr_test", ScopeLongTerm, "", "purple", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Statement, "purple") {
		t.Fatalf("expected purple memory, got %+v", entries)
	}

	deleted, err := store.Forget("usr_test", "The user likes purple.", ScopeLongTerm)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("expected one deleted row, got %d", deleted)
	}
	entries, err = store.ListMemories("usr_test", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no active memories, got %+v", entries)
	}
}

func TestStoreShortTermExpiry(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	_, err := store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeShortTerm, Category: "notes", Statement: "The user is testing expiry.", Evidence: "test", TTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	entries, err := store.ListMemories("usr_test", ScopeShortTerm, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected expired memory to be hidden, got %+v", entries)
	}
}

func TestStoreSessionContextIncludesSummaryAndRecentTurn(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fakeMemoryEmbedder{}, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.AppendSessionTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}

	ctx, err := store.BuildContext(context.Background(), "usr_test", "session-1", "purple memory", ContextOptions{RecentTurns: 2, ContextBudgetChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ctx.Block, "Stable User Profile") || strings.Contains(ctx.Block, "Current Session Summary") || !strings.Contains(ctx.Block, "Recent Exchanges") {
		t.Fatalf("unexpected context block:\n%s", ctx.Block)
	}
}

func TestStoreSessionContextIncludesToolAnnotationsAndScopesUser(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")

	ctx := context.Background()
	if err := store.AppendSessionTurn(ctx, "shared-session", "user-1", "first question", "first answer", []string{"github.get_issue", "web.search"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "shared-session", "user-2", "private question", "private answer", []string{"other.secret"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	retrieved, err := store.BuildContext(ctx, "user-1", "shared-session", "follow up", ContextOptions{RecentTurns: 4, ContextBudgetChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retrieved.Block, "Assistant: first answer\nTools used: github.get_issue, web.search") {
		t.Fatalf("tool annotation missing from context:\n%s", retrieved.Block)
	}
	if strings.Contains(retrieved.Block, "private") || strings.Contains(retrieved.Block, "other.secret") {
		t.Fatalf("context included another user's turn:\n%s", retrieved.Block)
	}
	if strings.Join(retrieved.RecentToolNames, ",") != "github.get_issue,web.search" {
		t.Fatalf("recent tool names = %+v", retrieved.RecentToolNames)
	}

	turns, err := store.RecentSessionTurns("user-2", "shared-session", 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].UserText != "private question" {
		t.Fatalf("unexpected scoped turns: %+v", turns)
	}
}

func TestBuildContextDoesNotEmbedQuery(t *testing.T) {
	embedder := &countingMemoryEmbedder{}
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), embedder, "fake-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "usr_test")

	if err := store.AppendSessionTurn(context.Background(), "session-1", "usr_test", "I like purple", "Noted.", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(context.Background(), "usr_test", SaveRequest{Scope: ScopeLongTerm, Category: "durable_preferences", Statement: "The user likes purple.", Evidence: "User said they like purple.", Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	seedEmbeddingCount := len(embedder.inputs)

	_, err = store.BuildContext(context.Background(), "usr_test", "session-1", "purple memory", ContextOptions{RecentTurns: 1, ContextBudgetChars: 4000})
	if err != nil {
		t.Fatal(err)
	}
	queryEmbeddings := embedder.inputs[seedEmbeddingCount:]
	if len(queryEmbeddings) != 0 {
		t.Fatalf("expected no query embeddings from automatic context, got %d: %+v", len(queryEmbeddings), queryEmbeddings)
	}
}

func TestMergeUsersTxCoalescesDuplicatesAndMovesData(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")

	ctx := context.Background()
	if _, err := store.sql.Exec(`INSERT INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at) VALUES ('winner', 'old winner', '2020-01-01T00:00:00Z', '2020-01-01T00:00:00Z'), ('loser', 'old loser', '2021-01-01T00:00:00Z', '2021-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	winnerDuplicate, err := store.SaveMemory(ctx, "winner", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Duplicate statement", Evidence: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	loserDuplicate, err := store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Duplicate statement", Evidence: "loser"})
	if err != nil {
		t.Fatal(err)
	}
	loserUnique, err := store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Unique statement", Evidence: "loser"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET supersedes_id = ? WHERE id = ?`, loserDuplicate.ID, loserUnique.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO memory_events (memory_id, event_type, created_at) VALUES (?, 'saved', ?)`, loserDuplicate.ID, formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`CREATE TABLE memory_entry_vectors (rowid INTEGER PRIMARY KEY, embedding BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO memory_entry_vectors (rowid, embedding) VALUES (?, X'02')`, loserDuplicate.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "session", "loser", "question", "answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "You are speaking with Winner."); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var duplicateCount, loserCount, loserVectorCount, winnerVectorCount int
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'winner' AND scope = ? AND statement_key = ?`, ScopeLongTerm, statementKey("Duplicate statement")).Scan(&duplicateCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'loser'`).Scan(&loserCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entry_vectors WHERE rowid = ?`, loserDuplicate.ID).Scan(&loserVectorCount); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entry_vectors WHERE rowid = ?`, winnerDuplicate.ID).Scan(&winnerVectorCount); err != nil {
		t.Fatal(err)
	}
	if duplicateCount != 1 || loserCount != 0 || loserVectorCount != 0 || winnerVectorCount != 1 {
		t.Fatalf("duplicate=%d loser entries=%d loser vectors=%d winner vectors=%d", duplicateCount, loserCount, loserVectorCount, winnerVectorCount)
	}
	var eventMemoryID, supersedesID int64
	if err := store.sql.QueryRow(`SELECT memory_id FROM memory_events WHERE event_type = 'saved'`).Scan(&eventMemoryID); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT supersedes_id FROM memory_entries WHERE id = ?`, loserUnique.ID).Scan(&supersedesID); err != nil {
		t.Fatal(err)
	}
	if eventMemoryID != winnerDuplicate.ID || supersedesID != winnerDuplicate.ID {
		t.Fatalf("event memory=%d supersedes=%d, want %d", eventMemoryID, supersedesID, winnerDuplicate.ID)
	}
	var turnOwner, intro, createdAt string
	if err := store.sql.QueryRow(`SELECT canonical_user_id FROM session_turns WHERE session_id = 'session'`).Scan(&turnOwner); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT intro, created_at FROM user_memory_profiles WHERE canonical_user_id = 'winner'`).Scan(&intro, &createdAt); err != nil {
		t.Fatal(err)
	}
	if turnOwner != "winner" || intro != "You are speaking with Winner." || createdAt != "2020-01-01T00:00:00Z" {
		t.Fatalf("turn owner=%q intro=%q created_at=%q", turnOwner, intro, createdAt)
	}
	var loserProfileCount int
	if err := store.sql.QueryRow(`SELECT count(*) FROM user_memory_profiles WHERE canonical_user_id = 'loser'`).Scan(&loserProfileCount); err != nil {
		t.Fatal(err)
	}
	if loserProfileCount != 0 {
		t.Fatalf("loser profile count = %d", loserProfileCount)
	}
}

func TestMergeUsersTxPreservesLoserOnlyVectorInVecTable(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	winner, err := store.SaveMemory(ctx, "winner", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Shared fact", Evidence: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	loser, err := store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Shared fact", Evidence: "loser", Embedding: []float64{0.1, 0.2}})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "winner intro"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var winnerVectors, loserVectors int
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entry_vectors WHERE rowid = ?`, winner.ID).Scan(&winnerVectors); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entry_vectors WHERE rowid = ?`, loser.ID).Scan(&loserVectors); err != nil {
		t.Fatal(err)
	}
	if winnerVectors != 1 || loserVectors != 0 {
		t.Fatalf("winner vectors=%d loser vectors=%d", winnerVectors, loserVectors)
	}
}

func TestMergeUsersTxRollbackLeavesDataUnchanged(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	if _, err := store.SaveMemory(context.Background(), "loser", SaveRequest{Scope: ScopeLongTerm, Statement: "Rollback memory"}); err != nil {
		t.Fatal(err)
	}

	tx, err := store.sql.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(context.Background(), tx, "winner", "loser", "winner intro"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	var loserEntries, winnerProfiles int
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'loser'`).Scan(&loserEntries); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM user_memory_profiles WHERE canonical_user_id = 'winner'`).Scan(&winnerProfiles); err != nil {
		t.Fatal(err)
	}
	if loserEntries != 1 || winnerProfiles != 0 {
		t.Fatalf("loser entries=%d winner profiles=%d", loserEntries, winnerProfiles)
	}
}

func TestStoreDoesNotRecreateStaleAccountUser(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "stale")
	if _, err := store.sql.Exec(`DELETE FROM account_users WHERE canonical_user_id = 'stale'`); err != nil {
		t.Fatal(err)
	}

	_, err := store.SaveMemory(context.Background(), "stale", SaveRequest{Statement: "Must not be saved"})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("stale user error = %v", err)
	}
	var accountCount int
	if err := store.sql.QueryRow(`SELECT count(*) FROM account_users WHERE canonical_user_id = 'stale'`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 0 {
		t.Fatalf("stale account was recreated")
	}
}

func seedAccountUsers(t *testing.T, store *Store, userIDs ...string) {
	t.Helper()
	now := formatTime(time.Now())
	for _, userID := range userIDs {
		if _, err := store.sql.Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, now, now); err != nil {
			t.Fatalf("seed account user %q: %v", userID, err)
		}
	}
}
