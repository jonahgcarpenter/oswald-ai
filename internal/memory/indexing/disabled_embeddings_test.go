package indexing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/global"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func TestDisabledEmbeddingsRetirePersistedVectorsAndRebuildOnReenable(t *testing.T) {
	for _, tc := range []struct{ name, model string }{{"unset or empty", ""}, {"whitespace", " \t "}} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "oswald.db")
			store := newLifecycleStoreAt(t, path, "user")
			quiet := config.NewLogger(config.LevelError)
			embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
			globalStore, err := global.NewStore(path, embedder, "model", quiet)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = globalStore.Close() })
			if _, err := memorytest.PublishMemory(ctx, store, "user", memorytest.MemoryFixture{Statement: "Initial synthetic memory."}); err != nil {
				t.Fatal(err)
			}
			if _, err := globalStore.Add(ctx, "Initial synthetic global fact."); err != nil {
				t.Fatal(err)
			}
			if err := NewService(store, globalStore, embedder, "model", quiet).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			var old []memory.DerivedIndexRevision
			for _, kind := range []string{memory.IndexKindMemoryVector, memory.IndexKindGlobalMemoryVector} {
				live, err := store.LiveIndexRevision(ctx, kind)
				if err != nil {
					t.Fatal(err)
				}
				building, err := store.CreateIndexRevision(ctx, kind, "llm_gateway", "model", 2)
				if err != nil {
					t.Fatal(err)
				}
				old = append(old, live, building)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if count, err := store.RetireVectorIndexRevisions(canceled); !errors.Is(err, context.Canceled) || count != 0 {
				t.Fatalf("canceled retirement count=%d err=%v", count, err)
			}
			for _, revision := range old {
				if _, err := store.LiveIndexRevision(ctx, revision.Kind); err != nil {
					t.Fatalf("cancellation retired a live vector: %v", err)
				}
			}
			// Restart with a client still available but no configured embedding model.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := globalStore.Close(); err != nil {
				t.Fatal(err)
			}
			forbidden := monitoringEmbedder(func(context.Context, llm.EmbedRequest) (*llm.EmbedResponse, error) {
				t.Error("disabled configuration invoked embedding provider")
				return nil, errors.New("disabled embedding canary")
			})
			store, err = memory.NewSQLiteStore(path, forbidden, model, quiet)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			globalStore, err = global.NewStore(path, forbidden, model, quiet)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = globalStore.Close() })
			// Retrieval is disabled even before the worker retires persisted vectors.
			_, recallStats := store.Recall(ctx, "user", "synthetic", memory.RecallRequest{})
			_, globalStats := globalStore.Search(ctx, "synthetic", 5)
			if recallStats.SemanticAvailable || globalStats.SemanticAvailable || !recallStats.LexicalAvailable || !globalStats.LexicalAvailable {
				t.Fatalf("disabled retrieval recall=%+v global=%+v", recallStats, globalStats)
			}
			added, err := memorytest.PublishMemory(ctx, store, "user", memorytest.MemoryFixture{Statement: "Memory added while embeddings disabled."})
			if err != nil {
				t.Fatal(err)
			}
			globalAdded, err := globalStore.Add(ctx, "Global fact added while embeddings disabled.")
			if err != nil {
				t.Fatal(err)
			}
			db, err := database.Open(path, quiet)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// Synthetic persisted retry fixture, immediately ready without sleeping.
			if _, err := db.SQL().Exec(`UPDATE durable_jobs SET state = 'retry', attempt_count = 100, available_at = '2000-01-01T00:00:00Z' WHERE job_kind = 'derived_index' AND state = 'queued'`); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			log := config.NewLogger(config.LevelInfo)
			log.SetOutput(&output)
			service := NewService(store, globalStore, forbidden, model, log)
			// The per-change guard also prevents calls if retirement has not run yet.
			for _, change := range []memory.DerivedIndexChange{
				{EntityKind: "memory", EntityID: added.ID, UserID: "user", Operation: "upsert"},
				{EntityKind: "global_memory", EntityID: globalAdded.Memory.ID, Operation: "upsert"},
			} {
				if err := service.applyChange(ctx, change); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := service.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			for _, revision := range old {
				var state string
				if err := db.SQL().QueryRow(`SELECT state FROM derived_index_revisions WHERE id = ?`, revision.ID).Scan(&state); err != nil || state != "retired" {
					t.Fatalf("old vector state=%s err=%v", state, err)
				}
				if _, err := store.LiveIndexRevision(ctx, revision.Kind); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("disabled vector still live: %v", err)
				}
			}
			for _, target := range []struct {
				kind string
				id   int64
			}{
				{memory.IndexKindMemoryFTS, added.ID}, {memory.IndexKindGlobalMemoryFTS, globalAdded.Memory.ID},
			} {
				live, err := store.LiveIndexRevision(ctx, target.kind)
				if err != nil {
					t.Fatal(err)
				}
				var count int
				if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM `+live.TableName+` WHERE rowid = ?`, target.id).Scan(&count); err != nil || count != 1 {
					t.Fatalf("lexical update count=%d err=%v", count, err)
				}
			}
			if _, err := store.LiveIndexRevision(ctx, memory.IndexKindTranscriptFTS); err != nil {
				t.Fatal(err)
			}
			var unfinished int
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND state IN ('queued', 'retry', 'running')`).Scan(&unfinished); err != nil || unfinished != 0 {
				t.Fatalf("unfinished indexing=%d err=%v", unfinished, err)
			}
			count := 0
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event["event"] == "index.vector.disabled" {
					count++
					if event["level"] != "info" || event["revision_count"] != float64(4) || event["status"] != "ok" || event["workload"] != "indexing" {
						t.Fatalf("disable measurement=%+v", event)
					}
				}
			}
			if count != 1 {
				t.Fatalf("disable measurement count=%d", count)
			}
			if err := NewService(store, globalStore, embedder, "model", quiet).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			for _, previous := range old {
				live, err := store.LiveIndexRevision(ctx, previous.Kind)
				if err != nil || live.Revision <= previous.Revision || live.IndexedCount != 2 || live.ExpectedCount != 2 {
					t.Fatalf("reenabled vector=%+v err=%v", live, err)
				}
			}
		})
	}
}
