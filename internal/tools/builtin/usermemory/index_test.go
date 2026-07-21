package usermemory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func TestDerivedIndexOutboxIdempotencyRetryAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := NewStore(path, config.NewLogger(config.LevelError))
	seedAccountUsers(t, store, "user")
	tx, err := store.sql.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueDerivedChangeTx(context.Background(), tx, "user", "memory", 42, "upsert", "same-mutation"); err != nil {
		t.Fatal(err)
	}
	if err := enqueueDerivedChangeTx(context.Background(), tx, "user", "memory", 42, "upsert", "same-mutation"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("idempotent count=%d err=%v", count, err)
	}
	change, err := store.ClaimDerivedIndexChange(context.Background(), "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RetryDerivedIndexChange(context.Background(), change, "offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ? AND job_kind = 'derived_index'`, formatTime(time.Now().Add(-time.Second)), change.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path, nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reclaimed, err := store.ClaimDerivedIndexChange(context.Background(), "restart", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Sequence != change.Sequence || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaimed=%+v initial=%+v", reclaimed, change)
	}
	if err := store.CompleteDerivedIndexChange(context.Background(), reclaimed.Sequence); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteIndexRecordCannotCrossTenant(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-a", "user-b")
	memory, err := store.SaveMemory(ctx, "user-b", SaveRequest{Scope: ScopeLongTerm, Statement: "Tenant B private memory"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user-b")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteIndexRecord(ctx, revision, memory.ID, "user-a"); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ? AND canonical_user_id = 'user-b'`, 1, memory.ID)
}

func TestStaleMemoryIndexWriteCannotRepublishAfterPrivacyMutation(t *testing.T) {
	for _, mutation := range []string{"forget", "delete", "erase_user"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
			defer store.Close() // nolint:errcheck
			seedAccountUsers(t, store, "user")
			memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "private stale content"})
			if err != nil {
				t.Fatal(err)
			}
			record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			switch mutation {
			case "forget":
				_, err = store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, "request", now, config.RetentionPolicy{ForgottenContentGrace: time.Hour})
			case "delete":
				_, err = store.DeleteMemory(ctx, "user", hashText("actor"), memory.ID, "request", now)
			case "erase_user":
				tx, beginErr := store.sql.BeginTx(ctx, nil)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				_, _, _, err = eraseUserTx(ctx, tx, "user", "", now)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); !errors.Is(err, ErrStaleIndexRecord) {
				t.Fatalf("write error = %v, want stale record", err)
			}
			assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, memory.ID)
		})
	}
}

func TestPrivacyMutationSerializesWithIndexRecheck(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "serialized private content"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	rechecked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.indexWriteHook = func(point string) {
		if point == "after_recheck" {
			once.Do(func() {
				close(rechecked)
				<-release
			})
		}
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- store.WriteMemoryIndexRecord(ctx, revision, record, nil) }()
	<-rechecked
	forgetDone := make(chan error, 1)
	go func() {
		_, err := store.ForgetMemory(ctx, "user", hashText("actor"), memory.ID, "request", time.Now().UTC(), config.RetentionPolicy{ForgottenContentGrace: time.Hour})
		forgetDone <- err
	}()
	select {
	case err := <-forgetDone:
		t.Fatalf("privacy transaction did not wait for index transaction: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-forgetDone; err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, memory.ID)
}

func TestStaleTranscriptIndexWriteCannotRepublishDeletedSession(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "private transcript", "ack", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET delivered_at = created_at WHERE id = ?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	record, err := store.TranscriptIndexRecordByID(ctx, turn.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSessionPrivacy(ctx, "user", hashText("actor"), "session", "request", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteTranscriptIndexRecord(ctx, revision, record); !errors.Is(err, ErrStaleIndexRecord) {
		t.Fatalf("write error = %v, want stale record", err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = ?`, 0, turn.ID)
}

func TestMaintenanceSkipsBuildingRevision(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO ` + revision.TableName + `(rowid, canonical_user_id, statement, evidence) VALUES (999, 'user', 'building', '')`); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if counts.RowsDeleted != 0 || counts.RevisionsDegraded != 0 {
		t.Fatalf("maintenance touched building revision: %+v", counts)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM `+revision.TableName+` WHERE rowid = 999`, 1)
	var state, code string
	if err := store.sql.QueryRow(`SELECT state, last_error_code FROM derived_index_revisions WHERE id = ?`, revision.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "building" || code != "" {
		t.Fatalf("building health changed: state=%q code=%q", state, code)
	}
}

func TestIndexMaintenanceRepairsOrphansAndDegradesMissingCoverage(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "canonical fact"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`INSERT INTO ` + revision.TableName + `(rowid, canonical_user_id, statement, evidence) VALUES (9999, 'user', 'orphan', '')`); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RowsDeleted != 1 || counts.RevisionsDegraded != 0 {
		t.Fatalf("orphan repair counts=%+v err=%v", counts, err)
	}
	if _, err := store.LiveIndexRevision(ctx, IndexKindMemoryFTS); err != nil {
		t.Fatalf("orphan repair displaced live revision: %v", err)
	}
	if _, err := store.sql.Exec(`DELETE FROM `+revision.TableName+` WHERE rowid = ?`, memory.ID); err != nil {
		t.Fatal(err)
	}
	counts, err = store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RevisionsDegraded != 1 {
		t.Fatalf("coverage counts=%+v err=%v", counts, err)
	}
	var state, code string
	if err := store.sql.QueryRow(`SELECT state, last_error_code FROM derived_index_revisions WHERE id = ?`, revision.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "live" || code != "coverage_mismatch" {
		t.Fatalf("state=%q code=%q", state, code)
	}
}

func TestRevisionValidationRejectsStaleCanonicalContent(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "original canonical fact"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.MemoryIndexRecordByID(ctx, memory.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET statement = 'updated canonical fact', updated_at = ? WHERE id = ?`, formatTime(time.Now().UTC().Add(time.Second)), memory.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err == nil {
		t.Fatal("stale shadow revision was published")
	}
}

func TestIndexMaintenanceNeverDropsLiveAndRetainsRetiredUntilDue(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	now := time.Now().UTC()
	first, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE derived_index_revisions SET updated_at = ? WHERE id = ?`, formatTime(now.Add(-48*time.Hour)), first.ID); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.MaintainDerivedIndexes(ctx, now, time.Hour, 100); err != nil || counts.TablesDropped != 0 {
		t.Fatalf("live cleanup counts=%+v err=%v", counts, err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, 1, first.TableName)

	second, err := store.CreateIndexRevision(ctx, IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE derived_index_revisions SET updated_at = ? WHERE id = ?`, formatTime(now.Add(-30*time.Minute)), first.ID); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.MaintainDerivedIndexes(ctx, now, time.Hour, 100); err != nil || counts.TablesDropped != 0 {
		t.Fatalf("early retired cleanup counts=%+v err=%v", counts, err)
	}
	if _, err := store.sql.Exec(`UPDATE derived_index_revisions SET updated_at = ? WHERE id = ?`, formatTime(now.Add(-time.Hour)), first.ID); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.MaintainDerivedIndexes(ctx, now, time.Hour, 100); err != nil || counts.TablesDropped != 1 {
		t.Fatalf("due retired cleanup counts=%+v err=%v", counts, err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, 0, first.TableName)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, 1, second.TableName)
	memory, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "delete after retired cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteMemory(ctx, "user", hashText("actor"), memory.ID, "request", now); err != nil {
		t.Fatalf("privacy deletion consulted dropped revision table: %v", err)
	}
}

func TestPartialLiveRevisionRemainsQueryableWhileUnhealthy(t *testing.T) {
	ctx := context.Background()
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	first, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "Project surviving-index remains active."})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveMemory(ctx, "user", SaveRequest{Scope: ScopeLongTerm, Statement: "Project missing-index remains active."})
	if err != nil {
		t.Fatal(err)
	}
	rebuildTestIndexes(t, store)
	live, err := store.LiveIndexRevision(ctx, IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`DELETE FROM `+live.TableName+` WHERE rowid = ?`, second.ID); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintainDerivedIndexes(ctx, time.Now().UTC(), time.Hour, 100)
	if err != nil || counts.RevisionsDegraded != 1 {
		t.Fatalf("maintenance counts=%+v err=%v", counts, err)
	}
	stillLive, err := store.LiveIndexRevision(ctx, IndexKindMemoryFTS)
	if err != nil || stillLive.ID != live.ID {
		t.Fatalf("serving pointer changed: live=%+v err=%v", stillLive, err)
	}
	results, stats := store.Recall(ctx, "user", "surviving-index", RecallRequest{TopK: 2})
	if !errors.Is(stats.LexicalError, ErrDerivedIndexDegraded) || len(results) != 1 || results[0].Entry.ID != first.ID {
		t.Fatalf("partial live recall results=%+v stats=%+v", results, stats)
	}
}

func rebuildTestIndexes(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	for _, kind := range []string{IndexKindMemoryFTS, IndexKindTranscriptFTS} {
		revision, err := store.CreateIndexRevision(ctx, kind, "sqlite_fts5", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if kind == IndexKindMemoryFTS {
			records, err := store.ActiveMemoryIndexRecords(ctx, 0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if err := store.WriteMemoryIndexRecord(ctx, revision, record, nil); err != nil {
					t.Fatal(err)
				}
			}
		} else {
			records, err := store.DeliveredTranscriptIndexRecords(ctx, 0, 10000)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if err := store.WriteTranscriptIndexRecord(ctx, revision, record); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
			t.Fatal(err)
		}
	}
	if store.embedder == nil || store.embedModel == "" {
		return
	}
	probe, err := store.embedder.Embed(ctx, llm.EmbedRequest{Model: store.embedModel, Input: "test probe"})
	if err != nil || probe == nil || len(probe.Embeddings) == 0 || len(probe.Embeddings[0]) == 0 {
		return
	}
	revision, err := store.CreateIndexRevision(ctx, IndexKindMemoryVector, "llm_gateway", store.embedModel, len(probe.Embeddings[0]))
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ActiveMemoryIndexRecords(ctx, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		response, err := store.embedder.Embed(ctx, llm.EmbedRequest{Model: store.embedModel, Input: memoryEmbeddingText(record.Scope, record.Category, record.Statement, record.Evidence)})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.WriteMemoryIndexRecord(ctx, revision, record, response.Embeddings[0]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
}
