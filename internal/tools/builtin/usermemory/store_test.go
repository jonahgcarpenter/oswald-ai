package usermemory

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
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

	rebuildTestIndexes(t, store)
	liveFTS, err := store.LiveIndexRevision(context.Background(), IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
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
	var erasedStatement, erasedEvidence string
	if err := store.sql.QueryRow(`SELECT statement, evidence FROM memory_entries WHERE id = ?`, entry.ID).Scan(&erasedStatement, &erasedEvidence); err != nil {
		t.Fatal(err)
	}
	var ftsCount int
	if err := store.sql.QueryRow(`SELECT count(*) FROM `+liveFTS.TableName+` WHERE rowid = ?`, entry.ID).Scan(&ftsCount); err != nil {
		t.Fatal(err)
	}
	if erasedStatement != "" || erasedEvidence != "" || ftsCount != 1 {
		t.Fatalf("forgotten content retained: statement=%q evidence=%q fts=%d", erasedStatement, erasedEvidence, ftsCount)
	}
	staleResults, staleStats := store.Recall(context.Background(), "usr_test", "purple", RecallRequest{TopK: 5})
	if staleStats.LexicalError != nil || len(staleResults) != 0 {
		t.Fatalf("stale derived row became observable: results=%+v stats=%+v", staleResults, staleStats)
	}
	entries, err = store.ListMemories("usr_test", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no active memories, got %+v", entries)
	}
}

func TestSaveMemoryUpsertKeepsTenantScopedIDAndVector(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1", "user-2")
	ctx := context.Background()
	first, err := store.SaveMemory(ctx, "user-1", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "User one fact.", Evidence: "first", Embedding: []float64{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveMemory(ctx, "user-2", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "User two private fact.", Evidence: "private", Embedding: []float64{0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.SaveMemory(ctx, "user-1", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "User one fact.", Evidence: "updated without embedding"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != first.ID || updated.UserID != "user-1" || updated.Statement != "User one fact." {
		t.Fatalf("upsert returned wrong tenant memory: %+v", updated)
	}
	var secondChanges int
	if err := store.sql.QueryRow(`SELECT count(*) FROM derived_index_changes WHERE entity_kind = 'memory' AND entity_id = ?`, second.ID).Scan(&secondChanges); err != nil {
		t.Fatal(err)
	}
	if secondChanges != 1 {
		t.Fatalf("foreign tenant derived change count = %d, want 1", secondChanges)
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
	if _, err := store.sql.Exec(`INSERT INTO memory_events (canonical_user_id, memory_id, event_type, created_at) VALUES ('loser', ?, 'saved', ?)`, loserDuplicate.ID, formatTime(time.Now())); err != nil {
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
	if duplicateCount != 1 || loserCount != 0 || loserVectorCount != 1 || winnerVectorCount != 0 {
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

func TestMergeUsersTxRebuildsWinnerVectorAsynchronously(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "test-embed", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	winner, err := store.SaveMemory(ctx, "winner", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Shared fact", Evidence: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "notes", Statement: "Shared fact", Evidence: "loser", Embedding: []float64{0.1, 0.2}})
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

	store.embedder = fixedRecallEmbedder{vector: []float64{0.1, 0.2}}
	rebuildTestIndexes(t, store)
	results, stats := store.Recall(ctx, "winner", "unmatched semantic query", RecallRequest{TopK: 2})
	if stats.SemanticError != nil || len(results) != 1 || results[0].Entry.ID != winner.ID {
		t.Fatalf("merged duplicate semantic recall results=%+v stats=%+v", results, stats)
	}
}

func TestMergeUsersTxPreservesFormationRowsAcrossDuplicatePublication(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	output := evaluatedFormationCandidate(t, "I use Go", "I use Go", "The user uses Go.", memoryformation.CategoryProjects)
	var winnerMemoryID, loserCandidateID int64
	for _, userID := range []string{"winner", "loser"} {
		candidate, created, err := store.ProposeCandidate(ctx, userID, CandidateProposal{Output: output, IdempotencyKey: "same-key", Source: FormationSource{RequestID: userID + "-request"}})
		if err != nil || !created {
			t.Fatalf("propose %s candidate=%+v created=%v err=%v", userID, candidate, created, err)
		}
		memory, err := store.PublishCandidate(ctx, userID, candidate.ID)
		if err != nil {
			t.Fatalf("publish %s: %v", userID, err)
		}
		if userID == "winner" {
			winnerMemoryID = memory.ID
		} else {
			loserCandidateID = candidate.ID
		}
	}

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "winner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var owner, key string
	var publishedID int64
	if err := store.sql.QueryRow(`SELECT canonical_user_id, idempotency_key, published_memory_id FROM memory_candidates WHERE id = ?`, loserCandidateID).Scan(&owner, &key, &publishedID); err != nil {
		t.Fatal(err)
	}
	if owner != "winner" || !strings.HasPrefix(key, "merge:loser:same-key:") || publishedID != winnerMemoryID {
		t.Fatalf("merged candidate owner=%q key=%q published=%d want=%d", owner, key, publishedID, winnerMemoryID)
	}
	for table, want := range map[string]int{"memory_candidates": 2, "memory_evidence": 4, "memory_formation_audit": 4} {
		var got int
		if err := store.sql.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE canonical_user_id = 'winner'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("merged %s count=%d want=%d", table, got, want)
		}
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

func TestMergeUsersTxPreservesProfilesAndCompactedSessionCollision(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()

	winnerMemory, err := store.SaveMemory(ctx, "winner", SaveRequest{Scope: ScopeLongTerm, Category: "projects", Statement: "Winner builds Atlas", Evidence: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	loserMemory, err := store.SaveMemory(ctx, "loser", SaveRequest{Scope: ScopeLongTerm, Category: "projects", Statement: "Loser builds Beacon", Evidence: "loser"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET profile_approved = 1 WHERE id IN (?, ?)`, winnerMemory.ID, loserMemory.ID); err != nil {
		t.Fatal(err)
	}
	winnerProfile, err := store.ResolveSessionProfile(ctx, "winner", "shared", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	loserProfile, err := store.ResolveSessionProfile(ctx, "loser", "shared", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	winnerFirst := appendDeliveredCompactionTurn(t, store, "winner", "shared", winnerProfile.Generation, "winner one")
	winnerLast := appendDeliveredCompactionTurn(t, store, "winner", "shared", winnerProfile.Generation, "winner two")
	loserFirst := appendDeliveredCompactionTurn(t, store, "loser", "shared", loserProfile.Generation, "loser one")
	loserLast := appendDeliveredCompactionTurn(t, store, "loser", "shared", loserProfile.Generation, "loser two")
	winnerSummary, winnerJob := publishMergeTestSummary(t, store, "winner", "shared", winnerProfile.Generation, winnerFirst, winnerLast, "winner checkpoint")
	loserSummary, loserJob := publishMergeTestSummary(t, store, "loser", "shared", loserProfile.Generation, loserFirst, loserLast, "loser checkpoint")
	if _, err := store.sql.Exec(`UPDATE session_compaction_jobs SET state = 'running', lease_owner = 'old-worker', lease_until = ? WHERE id = ?`, formatTime(time.Now().Add(time.Minute)), loserJob); err != nil {
		t.Fatal(err)
	}

	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeUsersTx(ctx, tx, "winner", "loser", "merged intro"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var activeGeneration int
	var activeProfileID int64
	if err := store.sql.QueryRow(`SELECT generation, profile_version_id FROM tenant_sessions WHERE canonical_user_id = 'winner' AND session_id = 'shared'`).Scan(&activeGeneration, &activeProfileID); err != nil {
		t.Fatal(err)
	}
	if activeGeneration <= winnerProfile.Generation || activeProfileID != loserProfile.VersionID {
		t.Fatalf("active generation=%d profile=%d, winner generation=%d loser profile=%d", activeGeneration, activeProfileID, winnerProfile.Generation, loserProfile.VersionID)
	}
	var turnCount, summaryCount, sourceCount, jobCount, oldProfileCount int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'winner' AND session_id = 'shared'`:                                                                                    &turnCount,
		`SELECT COUNT(*) FROM session_summaries WHERE canonical_user_id = 'winner' AND id IN (` + fmt.Sprint(winnerSummary.ID) + `,` + fmt.Sprint(loserSummary.ID) + `)`:                     &summaryCount,
		`SELECT COUNT(*) FROM session_summary_sources WHERE canonical_user_id = 'winner'`:                                                                                                    &sourceCount,
		`SELECT COUNT(*) FROM session_compaction_jobs WHERE canonical_user_id = 'winner' AND id IN (` + fmt.Sprint(winnerJob) + `,` + fmt.Sprint(loserJob) + `)`:                             &jobCount,
		`SELECT COUNT(*) FROM tenant_profile_versions WHERE canonical_user_id = 'winner' AND id IN (` + fmt.Sprint(winnerProfile.VersionID) + `,` + fmt.Sprint(loserProfile.VersionID) + `)`: &oldProfileCount,
	} {
		if err := store.sql.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if turnCount != 4 || summaryCount != 2 || sourceCount != 4 || jobCount != 2 || oldProfileCount != 2 {
		t.Fatalf("turns=%d summaries=%d sources=%d jobs=%d profiles=%d", turnCount, summaryCount, sourceCount, jobCount, oldProfileCount)
	}
	var jobState, leaseOwner string
	var leaseUntil sql.NullString
	var artifactSummaryID int64
	if err := store.sql.QueryRow(`SELECT state, lease_owner, lease_until, artifact_summary_id FROM session_compaction_jobs WHERE id = ?`, loserJob).Scan(&jobState, &leaseOwner, &leaseUntil, &artifactSummaryID); err != nil {
		t.Fatal(err)
	}
	if jobState != "retry" || leaseOwner != "" || leaseUntil.Valid || artifactSummaryID != loserSummary.ID {
		t.Fatalf("loser job state=%q owner=%q lease=%v artifact=%d", jobState, leaseOwner, leaseUntil, artifactSummaryID)
	}
	latest, err := store.LatestSessionSummary(ctx, "winner", "shared", activeGeneration)
	if err != nil || latest.ID != loserSummary.ID || latest.Narrative != "loser checkpoint" {
		t.Fatalf("latest merged summary=%+v err=%v", latest, err)
	}
	resolved, err := store.ResolveSessionProfile(ctx, "winner", "shared", time.Hour)
	if err != nil || resolved.VersionID != loserProfile.VersionID {
		t.Fatalf("frozen merged profile=%+v err=%v", resolved, err)
	}
	contextResult, err := store.BuildContext(ctx, "winner", "shared", "Beacon", ContextOptions{Generation: activeGeneration, RecentTurns: 10, ContextBudgetChars: 8000})
	if err != nil || !strings.Contains(contextResult.Block, "loser two") {
		t.Fatalf("merged prompt context=%q err=%v", contextResult.Block, err)
	}
	rebuildTestIndexes(t, store)
	transcript, err := store.SearchTranscript(ctx, "winner", "shared", activeGeneration, "loser two", 5)
	if err != nil || len(transcript) == 0 || transcript[0].TurnID != loserLast {
		t.Fatalf("merged transcript=%+v err=%v", transcript, err)
	}
	recalled, _ := store.Recall(ctx, "winner", "Loser builds Beacon", RecallRequest{TopK: 5})
	if len(recalled) == 0 || recalled[0].Entry.ID != loserMemory.ID {
		t.Fatalf("merged recall=%+v", recalled)
	}
	memories, err := store.ListMemories("winner", "", "", 10)
	if err != nil || len(memories) != 2 {
		t.Fatalf("merged recall source memories=%+v err=%v", memories, err)
	}
}

func publishMergeTestSummary(t *testing.T, store *Store, userID, sessionID string, generation int, fromID, throughID int64, narrative string) (SessionSummary, int64) {
	t.Helper()
	jobID, err := store.EnqueueSessionCompactionJob(context.Background(), userID, sessionID, generation, fromID, throughID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimSessionCompactionJob(context.Background(), "merge-test", time.Minute)
	if err != nil || job.ID != jobID {
		t.Fatalf("claim merge job=%+v want=%d err=%v", job, jobID, err)
	}
	if err := store.SaveSessionCompactionArtifact(context.Background(), job, SummaryArtifact{Narrative: narrative, GenerationModel: "test-model", GeneratorVersion: "test-v1"}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.PublishSessionSummary(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteSessionCompactionJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	return summary, jobID
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
