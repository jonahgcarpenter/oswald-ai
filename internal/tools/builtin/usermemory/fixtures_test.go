package usermemory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func newHandlerTestStore(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	store, err := memory.NewSQLiteStore(path, nil, "", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := database.Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store, db.SQL()
}

func seedHandlerUser(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO account_users (canonical_user_id) VALUES (?)`, userID); err != nil {
		t.Fatal(err)
	}
}

func bindTranscriptTestSession(t *testing.T, store *memory.Store, userID, sessionID string) int {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), userID, sessionID, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return profile.Generation
}

func insertTranscriptTestTurn(t *testing.T, store *memory.Store, userID, sessionID string, generation int, userText, assistantText string, delivered bool, ttl time.Duration) {
	t.Helper()
	turn, err := memorytest.AppendPendingTurn(context.Background(), store, sessionID, userID, generation, userText, assistantText, nil, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		if err := store.MarkSessionTurnDelivered(context.Background(), userID, turn.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func rebuildHandlerIndexes(t *testing.T, store *memory.Store) {
	t.Helper()
	ctx := context.Background()
	revision, err := store.CreateIndexRevision(ctx, memory.IndexKindTranscriptFTS, "sqlite_fts5", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.DeliveredTranscriptIndexRecords(ctx, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := store.WriteTranscriptIndexRecord(ctx, revision, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ValidateAndPublishIndexRevision(ctx, revision.ID); err != nil {
		t.Fatal(err)
	}
}
