package usermemory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

func TestMemoryHandlersUsePrincipalCanonicalUser(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "", log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	seedAccountUsers(t, store, "usr_1", "usr_2")

	userOne := requestctx.WithMetadata(principalContext("usr_1", "same-external"), requestctx.Metadata{RequestID: "req", SessionID: "session", Model: "test", CurrentUserText: "Remember that I like purple."})
	userTwo := principalContext("usr_2", "same-external")
	save := NewSaveHandler(store, log)
	list := NewListHandler(store, log)
	forget := NewForgetHandler(store, log)

	if _, err := save(userOne, map[string]interface{}{
		"scope":      "long_term",
		"category":   "durable_preferences",
		"statement":  "The user likes purple.",
		"confidence": 1.0,
		"importance": 3,
	}); err != nil {
		t.Fatalf("save memory: %v", err)
	}
	var candidateID int64
	if err := store.sql.QueryRow(`SELECT id FROM memory_candidates WHERE canonical_user_id = 'usr_1' AND source_request_id = 'req'`).Scan(&candidateID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishCandidate(context.Background(), "usr_1", candidateID); err != nil {
		t.Fatal(err)
	}

	otherList, err := list(userTwo, map[string]interface{}{})
	if err != nil {
		t.Fatalf("list other user: %v", err)
	}
	if otherList != "No active memories found for this user." {
		t.Fatalf("other user list = %q", otherList)
	}
	if result, err := forget(userTwo, map[string]interface{}{"statement": "The user likes purple."}); err != nil || result != "No matching active memories were found." {
		t.Fatalf("other user forget result=%q err=%v", result, err)
	}

	ownerList, err := list(userOne, map[string]interface{}{})
	if err != nil {
		t.Fatalf("list owner: %v", err)
	}
	if !strings.Contains(ownerList, "I like purple.") {
		t.Fatalf("owner memory missing after other user forget: %q", ownerList)
	}
}

func TestMemoryHandlersRejectMissingPrincipal(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "", log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	list := NewListHandler(store, log)
	for name, ctx := range map[string]context.Context{
		"missing": context.Background(),
		"invalid": requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "usr_1", Gateway: "websocket", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway}),
	} {
		if _, err := list(ctx, map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "no user identity") {
			t.Fatalf("%s principal error = %v", name, err)
		}
	}
}

func TestMemorySearchReportsTotalAndPartialDegradation(t *testing.T) {
	log := config.NewLogger(config.LevelError)
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{vector: []float64{1, 0}}, "test-embed", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seedAccountUsers(t, store, "usr_1")
	_, err = store.SaveMemory(context.Background(), "usr_1", SaveRequest{Scope: ScopeLongTerm, Category: "identity", Statement: "The user lives in Porto.", Evidence: "user statement", Embedding: []float64{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`DROP TABLE memory_entries_fts`); err != nil {
		t.Fatal(err)
	}
	search := NewSearchHandler(store, log)
	result, err := search(principalContext("usr_1", "alice"), map[string]interface{}{"query": "Where is home?"})
	if err != nil || !strings.Contains(result, "partially degraded") || !strings.Contains(result, "Porto") {
		t.Fatalf("partial search result=%q err=%v", result, err)
	}

	store.embedder = nil
	if _, err := search(principalContext("usr_1", "alice"), map[string]interface{}{"query": "Porto"}); err == nil || !strings.Contains(err.Error(), "indexes unavailable") {
		t.Fatalf("total degradation error = %v", err)
	}
}

func principalContext(userID, externalID string) context.Context {
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: externalID, Assurance: identity.AssuranceSelfAsserted}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	return requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req", SessionID: "session", Model: "test"})
}
