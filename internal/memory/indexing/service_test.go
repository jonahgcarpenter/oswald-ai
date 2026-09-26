package indexing

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

type countingEmbedder struct{ calls int }

func (e *countingEmbedder) Embed(context.Context, llm.EmbedRequest) (*llm.EmbedResponse, error) {
	e.calls++
	return nil, errors.New("fact embedding should not be called")
}

func TestFactIndexesRetiredAndJobsAcknowledgedWithoutEmbedding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "user")
	embedder := &countingEmbedder{}
	globalStore, err := global.NewStore(path, embedder, "model", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = globalStore.Close() })
	// Start with empty live revisions and unfinished shadows, then enqueue
	// changes against them before the worker retires both generations.
	for _, kind := range []string{memory.IndexKindMemoryFTS, memory.IndexKindMemoryVector, memory.IndexKindGlobalMemoryFTS, memory.IndexKindGlobalMemoryVector} {
		provider, model, dimension := "sqlite_fts5", "", 0
		if kind == memory.IndexKindMemoryVector || kind == memory.IndexKindGlobalMemoryVector {
			provider, model, dimension = "llm_gateway", "model", 2
		}
		live, err := store.CreateIndexRevision(ctx, kind, provider, model, dimension)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ValidateAndPublishIndexRevision(ctx, live.ID); err != nil {
			t.Fatal(err)
		}
		// An unfinished shadow must not resume on the next cycle.
		if _, err := store.CreateIndexRevision(ctx, kind, provider, model, dimension); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := globalStore.Add(ctx, "A searchable global testing fact."); err != nil {
		t.Fatal(err)
	}
	fact, err := memorytest.PublishMemory(ctx, store, "user", memorytest.MemoryFixture{Scope: memory.ScopeLongTerm, Statement: "An unused indexed fact."})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{
		UserID: "user", SessionID: "session", Generation: profile.Generation,
		UserText: "Transcript marker", AssistantText: "Transcript answer", TTL: time.Hour,
		Pressure: memory.SessionPromptPressure{Tokens: 1, Limit: 100, Version: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // nolint:errcheck
	// Simulate persisted pre-upgrade queued and retry fact work.
	if _, err := db.SQL().Exec(`INSERT INTO durable_jobs(job_kind, idempotency_key, canonical_user_id, entity_kind, entity_id, operation, available_at, updated_at) VALUES ('derived_index', 'legacy-memory', 'user', 'memory', ?, 'upsert', ?, ?)`, fact.ID, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`UPDATE durable_jobs SET state = 'retry', available_at = ? WHERE job_kind = 'derived_index' AND entity_kind = 'global_memory'`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, globalStore, embedder, "model", config.NewLogger(config.LevelError))
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if embedder.calls != 0 {
		t.Fatalf("fact embedding calls = %d", embedder.calls)
	}
	for _, kind := range []string{memory.IndexKindMemoryFTS, memory.IndexKindMemoryVector, memory.IndexKindGlobalMemoryFTS, memory.IndexKindGlobalMemoryVector} {
		if _, err := store.LiveIndexRevision(ctx, kind); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s remains live: %v", kind, err)
		}
		if _, err := store.BuildingIndexRevision(ctx, kind); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s remains building: %v", kind, err)
		}
	}
	var pending, completed int
	var indexed int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind IN ('memory', 'global_memory') AND state IN ('queued', 'retry', 'running')`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind IN ('memory', 'global_memory') AND state = 'succeeded'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || completed < 2 {
		t.Fatalf("fact jobs pending=%d completed=%d", pending, completed)
	}
	var reconciledFacts int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind IN ('memory', 'global_memory') AND idempotency_key LIKE 'reconcile:%'`).Scan(&reconciledFacts); err != nil || reconciledFacts != 0 {
		t.Fatalf("fact jobs created by reconciliation = %d, err = %v", reconciledFacts, err)
	}
	transcript, err := store.LiveIndexRevision(ctx, memory.IndexKindTranscriptFTS)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM `+transcript.TableName+` WHERE rowid = ?`, turn.ID).Scan(&indexed); err != nil || indexed != 1 {
		t.Fatalf("transcript FTS rows = %d, err = %v", indexed, err)
	}
	for _, kind := range []string{memory.IndexKindMemoryFTS, memory.IndexKindMemoryVector, memory.IndexKindGlobalMemoryFTS, memory.IndexKindGlobalMemoryVector} {
		var state string
		if err := db.SQL().QueryRow(`SELECT state FROM derived_index_revisions WHERE index_kind = ? AND revision = 1`, kind).Scan(&state); err != nil || state != "retired" {
			t.Fatalf("%s live revision state = %q, err = %v", kind, state, err)
		}
		if err := db.SQL().QueryRow(`SELECT state FROM derived_index_revisions WHERE index_kind = ? AND revision = 2`, kind).Scan(&state); err != nil || state != "failed" {
			t.Fatalf("%s shadow revision state = %q, err = %v", kind, state, err)
		}
		for _, revision := range []string{"r1", "r2"} {
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM derived_index_` + kind + `_` + revision).Scan(&indexed); err != nil || indexed != 0 {
				t.Fatalf("%s %s physical rows = %d, err = %v", kind, revision, indexed, err)
			}
		}
	}
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if embedder.calls != 0 {
		t.Fatalf("subsequent cycle embedded fact content: %d", embedder.calls)
	}
}

func newLifecycleStore(t *testing.T, users ...string) *memory.Store {
	t.Helper()
	return newLifecycleStoreAt(t, filepath.Join(t.TempDir(), "oswald.db"), users...)
}

func newLifecycleStoreAt(t *testing.T, path string, users ...string) *memory.Store {
	t.Helper()
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES (?)`, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewSQLiteStore(path, nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
