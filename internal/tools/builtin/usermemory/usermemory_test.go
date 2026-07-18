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

	userOne := principalContext("usr_1", "same-external")
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
	if !strings.Contains(ownerList, "The user likes purple.") {
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

func principalContext(userID, externalID string) context.Context {
	principal := identity.Principal{CanonicalUserID: userID, Gateway: "websocket", ExternalID: externalID, Assurance: identity.AssuranceSelfAsserted}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	return requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req", SessionID: "session", Model: "test"})
}
