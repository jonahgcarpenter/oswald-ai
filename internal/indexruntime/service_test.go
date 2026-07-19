package indexruntime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

type lifecycleEmbedder struct {
	mu          sync.Mutex
	dimensions  map[string]int
	failContent bool
	hook        func()
	hooked      bool
}

func TestMissingLiveTableTriggersShadowRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "user")
	if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "Rebuild missing physical table."}); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, "", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`DROP TABLE ` + old.TableName); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Revision <= old.Revision || rebuilt.IndexedCount != 1 {
		t.Fatalf("missing table was not rebuilt: old=%+v rebuilt=%+v", old, rebuilt)
	}
}

func TestMaintenanceDuringBuildDoesNotBlockPublication(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "Concurrent maintenance build."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
	embedder.hook = func() {
		if _, err := store.MaintenanceSweep(context.Background(), time.Now().UTC(), config.RetentionPolicy{RetiredIndexRetention: time.Hour, BatchSize: 100}); err != nil {
			t.Errorf("maintenance during build: %v", err)
		}
	}
	if err := NewService(store, embedder, "model", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil || live.State != "live" || live.IndexedCount != 1 {
		t.Fatalf("concurrent maintenance blocked publication: live=%+v err=%v", live, err)
	}
}

func (f *lifecycleEmbedder) Embed(_ context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error) {
	f.mu.Lock()
	dimension := f.dimensions[req.Model]
	isProbe := req.Input == "derived index dimension probe"
	fail := f.failContent && !isProbe
	hook := f.hook
	shouldHook := !isProbe && hook != nil && !f.hooked
	if shouldHook {
		f.hooked = true
	}
	f.mu.Unlock()
	if shouldHook {
		hook()
	}
	if fail {
		return nil, errors.New("embedding unavailable")
	}
	if dimension == 0 {
		return nil, errors.New("unknown model")
	}
	vector := make([]float64, dimension)
	vector[0] = 1
	return &llm.EmbedResponse{Model: req.Model, Embeddings: [][]float64{vector}}, nil
}

func TestVectorRevisionModelAndDimensionLifecycle(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Category: "projects", Statement: "Project Atlas is active."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model-a": 2, "model-b": 3}}
	service := NewService(store, embedder, "model-a", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if first.Model != "model-a" || first.Dimension != 2 || first.ExpectedCount != 1 || first.IndexedCount != 1 {
		t.Fatalf("first revision=%+v", first)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	same, _ := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if same.Revision != first.Revision {
		t.Fatalf("same configuration rebuilt revision: %d -> %d", first.Revision, same.Revision)
	}
	service = NewService(store, embedder, "model-b", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision <= first.Revision || changed.Model != "model-b" || changed.Dimension != 3 {
		t.Fatalf("changed revision=%+v", changed)
	}
}

func TestMatchingLegacyVectorRevisionRebuildsToCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "user")
	if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "Canonical vector record."}); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`CREATE VIRTUAL TABLE memory_entry_vectors_v2 USING vec0(canonical_user_id text, embedding_model text, scope text, category text, embedding float[2]); INSERT INTO derived_index_revisions(index_kind, provider, model, dimension, schema_version, revision, table_name, state, created_at, updated_at) VALUES ('memory_vector', 'llm_gateway', 'model', 2, 1, 1, 'memory_entry_vectors_v2', 'live', datetime('now'), datetime('now'))`); err != nil {
		db.Close() // nolint:errcheck
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
	if err := NewService(store, embedder, "model", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if live.SchemaVersion != 2 || live.TableName == "memory_entry_vectors_v2" || live.IndexedCount != 1 {
		t.Fatalf("legacy vector revision was not rebuilt: %+v", live)
	}
}

func TestWriteArrivingDuringVectorBuildIsReconciled(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "First canonical record."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"model": 2}}
	embedder.hook = func() {
		if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "Record written during build."}); err != nil {
			t.Errorf("write during build: %v", err)
		}
	}
	service := NewService(store, embedder, "model", config.NewLogger(config.LevelError))
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if live.ExpectedCount != 2 || live.IndexedCount != 2 {
		t.Fatalf("live coverage=%+v", live)
	}
}

func TestWriteDuringModelChangeUpdatesOldLiveAndNewShadow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newLifecycleStoreAt(t, path, "user")
	if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "First canonical record."}); err != nil {
		t.Fatal(err)
	}
	embedder := &lifecycleEmbedder{dimensions: map[string]int{"old": 2, "new": 3}}
	if err := NewService(store, embedder, "old", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	embedder.hook = func() {
		if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "Record written during model change."}); err != nil {
			t.Errorf("write during model change: %v", err)
		}
	}
	if err := NewService(store, embedder, "new", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // nolint:errcheck
	var oldRows int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + old.TableName).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 2 {
		t.Fatalf("old live revision row count = %d, want 2", oldRows)
	}
	newLive, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if newLive.Model != "new" || newLive.IndexedCount != 2 {
		t.Fatalf("new live revision = %+v, want model new with 2 rows", newLive)
	}
}

func TestFailedShadowBuildPreservesOldLiveRevision(t *testing.T) {
	store := newLifecycleStore(t, "user")
	if _, err := store.SaveMemory(context.Background(), "user", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "Stable canonical record."}); err != nil {
		t.Fatal(err)
	}
	good := &lifecycleEmbedder{dimensions: map[string]int{"old": 2}}
	if err := NewService(store, good, "old", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, _ := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	failing := &lifecycleEmbedder{dimensions: map[string]int{"new": 3}, failContent: true}
	if err := NewService(store, failing, "new", config.NewLogger(config.LevelError)).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveIndexRevision(context.Background(), usermemory.IndexKindMemoryVector)
	if err != nil {
		t.Fatal(err)
	}
	if live.ID != old.ID || live.Model != "old" {
		t.Fatalf("failed build replaced live revision: old=%+v live=%+v", old, live)
	}
	health, err := store.DerivedIndexHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for _, revision := range health {
		failed = failed || (revision.Kind == usermemory.IndexKindMemoryVector && revision.State == "failed")
	}
	if !failed {
		t.Fatalf("failed shadow revision missing from health: %+v", health)
	}
}

func TestRevisionValidationRejectsCrossTenantAndOrphanRows(t *testing.T) {
	store := newLifecycleStore(t, "user-a", "user-b")
	memory, err := store.SaveMemory(context.Background(), "user-a", usermemory.SaveRequest{Scope: usermemory.ScopeLongTerm, Statement: "Tenant A secret."})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.CreateIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(context.Background(), revision, usermemory.MemoryIndexRecord{ID: memory.ID, UserID: "user-b", Statement: "wrong owner"}, nil); !errors.Is(err, usermemory.ErrStaleIndexRecord) {
		t.Fatalf("cross-tenant write error = %v, want stale record", err)
	}
	revision, err = store.CreateIndexRevision(context.Background(), usermemory.IndexKindMemoryFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMemoryIndexRecord(context.Background(), revision, usermemory.MemoryIndexRecord{ID: 99999, UserID: "user-a", Statement: "orphan"}, nil); !errors.Is(err, usermemory.ErrStaleIndexRecord) {
		t.Fatalf("orphan write error = %v, want stale record", err)
	}
}

func newLifecycleStore(t *testing.T, users ...string) *usermemory.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oswald.db")
	return newLifecycleStoreAt(t, path, users...)
}

func newLifecycleStoreAt(t *testing.T, path string, users ...string) *usermemory.Store {
	t.Helper()
	db, err := database.Open(path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id, created_at, updated_at) VALUES (?, datetime('now'), datetime('now'))`, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := usermemory.NewSQLiteStore(path, nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
