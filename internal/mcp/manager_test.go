package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestManagerUserMergeCommittedInvalidatesAffectedSessions(t *testing.T) {
	manager := NewManagerFromStore(nil, config.NewLogger(config.LevelError))
	closed := map[string]int{}
	addSession := func(cfg ServerConfig) {
		manager.sessions[scopeKey(cfg)] = &server{config: cfg, close: func() error {
			closed[cfg.OwnerUserID]++
			return nil
		}}
	}
	addSession(ServerConfig{Scope: ScopeUser, OwnerUserID: "winner", Name: "winner-server"})
	addSession(ServerConfig{Scope: ScopeUser, OwnerUserID: "loser", Name: "loser-server"})
	addSession(ServerConfig{Scope: ScopeUser, OwnerUserID: "other", Name: "other-server"})
	addSession(ServerConfig{Scope: ScopeGlobal, Name: "global-server"})

	manager.UserMergeCommitted("winner", "loser")

	if closed["winner"] != 1 || closed["loser"] != 1 {
		t.Fatalf("affected close counts = %+v", closed)
	}
	if len(manager.sessions) != 2 {
		t.Fatalf("remaining sessions = %d, want 2", len(manager.sessions))
	}
	if manager.sessions[ScopeUser+":other:other-server"] == nil || manager.sessions[ScopeGlobal+":global-server"] == nil {
		t.Fatalf("unrelated sessions were invalidated: %+v", manager.sessions)
	}
}

func TestManagerUserDeleteCommittedInvalidatesOwnedSessions(t *testing.T) {
	manager := NewManagerFromStore(nil, config.NewLogger(config.LevelError))
	closed := 0
	deleted := ServerConfig{Scope: ScopeUser, OwnerUserID: "deleted", Name: "owned"}
	other := ServerConfig{Scope: ScopeUser, OwnerUserID: "other", Name: "other"}
	manager.sessions[scopeKey(deleted)] = &server{config: deleted, close: func() error { closed++; return nil }}
	manager.sessions[scopeKey(other)] = &server{config: other}

	manager.UserDeleteCommitted("deleted")

	if closed != 1 || manager.sessions[scopeKey(deleted)] != nil || manager.sessions[scopeKey(other)] == nil {
		t.Fatalf("delete invalidation closed=%d sessions=%+v", closed, manager.sessions)
	}
}

func TestManagerUserMergeCommittedRejectsStaleConnectionResult(t *testing.T) {
	manager := NewManagerFromStore(nil, config.NewLogger(config.LevelError))
	cfg := ServerConfig{Scope: ScopeUser, OwnerUserID: "loser", Name: "server"}
	generation := manager.userGenerations[cfg.OwnerUserID]

	manager.UserMergeCommitted("winner", "loser")
	manager.rememberError(scopeKey(cfg), cfg, generation, errTestConnection)

	if manager.sessions[scopeKey(cfg)] != nil {
		t.Fatal("stale connection result was cached after user merge")
	}
}

func TestManagerRejectsConfigLoadedBeforeUserMerge(t *testing.T) {
	store := testStore(t)
	addTestUsers(t, store, "winner", "loser")
	ctx := context.Background()
	stale, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "loser", Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/mcp"})
	if err != nil {
		t.Fatalf("save stale config: %v", err)
	}
	tx, err := store.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin merge: %v", err)
	}
	if err := store.MergeUsersTx(ctx, tx, "winner", "loser"); err != nil {
		t.Fatalf("merge config: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit merge: %v", err)
	}
	manager := NewManagerFromStore(store, config.NewLogger(config.LevelError))
	manager.UserMergeCommitted("winner", "loser")

	if _, err := manager.ensureConnected(ctx, stale); err == nil || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatalf("stale config connection error=%v", err)
	}
}

var errTestConnection = &testConnectionError{}

type testConnectionError struct{}

func (*testConnectionError) Error() string { return "test connection error" }
