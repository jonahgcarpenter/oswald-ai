package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"

	_ "github.com/mattn/go-sqlite3"
)

type staticResolver map[string][]string

func (r staticResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	_ = ctx
	return r[host], nil
}

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "oswald.db"), "12345678901234567890123456789012", config.NewLogger(config.LevelError).Server("test"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	store.SetResolverForTest(staticResolver{"example.com": {"93.184.216.34"}, "private.example": {"10.0.0.1"}})
	t.Cleanup(func() { store.Close() })
	return store
}

func addTestUsers(t *testing.T, store *Store, userIDs ...string) {
	t.Helper()
	for _, userID := range userIDs {
		if _, err := store.db.SQL().Exec(`INSERT INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("add test user %s: %v", userID, err)
		}
	}
}

func TestStoreEncryptsURLAndHeadersAtRest(t *testing.T) {
	store := testStore(t)
	addTestUsers(t, store, "user_1")
	_, err := store.Save(context.Background(), ServerConfig{
		Scope:       ScopeUser,
		OwnerUserID: "user_1",
		Name:        "home",
		Transport:   TransportStreamableHTTP,
		URL:         "https://example.com/mcp?token=secret",
		Headers:     map[string]string{"Authorization": "Bearer secret"},
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("save config: %v", err)
	}

	row := store.db.SQL().QueryRow(`SELECT url_ciphertext, url_host_hash, headers_ciphertext FROM mcp_servers WHERE name = 'home'`)
	var urlCiphertext, hostHash, headersCiphertext string
	if err := row.Scan(&urlCiphertext, &hostHash, &headersCiphertext); err != nil {
		t.Fatalf("read stored ciphertext: %v", err)
	}
	joined := urlCiphertext + " " + hostHash + " " + headersCiphertext
	for _, leaked := range []string{"example.com", "/mcp", "secret", "Bearer"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("stored ciphertext leaked %q: %s", leaked, joined)
		}
	}

	loaded, ok, err := store.Get(context.Background(), ScopeUser, "user_1", "home")
	if err != nil || !ok {
		t.Fatalf("load config ok=%v err=%v", ok, err)
	}
	if loaded.URL != "https://example.com/mcp?token=secret" || loaded.Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("unexpected decrypted config: %+v", loaded)
	}
}

func TestStoreScopesServersAndRejectsGlobalNameCollision(t *testing.T) {
	store := testStore(t)
	addTestUsers(t, store, "user_1")
	ctx := context.Background()
	if _, err := store.Save(ctx, ServerConfig{Scope: ScopeGlobal, Name: "github", Transport: TransportStreamableHTTP, URL: "https://example.com/mcp", Enabled: true}); err != nil {
		t.Fatalf("save global: %v", err)
	}
	if _, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "user_1", Name: "github", Transport: TransportStreamableHTTP, URL: "https://example.com/user", Enabled: true}); err == nil {
		t.Fatal("expected user/global name collision")
	}
	if _, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "user_1", Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/user", Enabled: true}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	user1, err := store.ListForUser(ctx, "user_1")
	if err != nil {
		t.Fatalf("list user1: %v", err)
	}
	if len(user1) != 2 {
		t.Fatalf("user1 visible count = %d, want 2", len(user1))
	}
	user2, err := store.ListForUser(ctx, "user_2")
	if err != nil {
		t.Fatalf("list user2: %v", err)
	}
	if len(user2) != 1 || user2[0].Name != "github" {
		t.Fatalf("unexpected user2 configs: %+v", user2)
	}
}

func TestStoreMergeUsersTxReencryptsForWinner(t *testing.T) {
	store := testStore(t)
	addTestUsers(t, store, "winner", "loser")
	ctx := context.Background()
	saved, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "loser", Name: "home", Type: "custom", Transport: TransportStreamableHTTP, URL: "https://example.com/mcp?token=secret", Headers: map[string]string{"Authorization": "Bearer secret"}, Enabled: true})
	if err != nil {
		t.Fatalf("save loser config: %v", err)
	}

	tx, err := store.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin merge: %v", err)
	}
	if err := store.MergeUsersTx(ctx, tx, "winner", "loser"); err != nil {
		tx.Rollback() // nolint:errcheck
		t.Fatalf("merge users: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit merge: %v", err)
	}

	loaded, ok, err := store.Get(ctx, ScopeUser, "winner", "home")
	if err != nil || !ok {
		t.Fatalf("load transferred config ok=%v err=%v", ok, err)
	}
	if loaded.ID != saved.ID || loaded.Type != saved.Type || loaded.Transport != saved.Transport || loaded.Enabled != saved.Enabled || !loaded.CreatedAt.Equal(saved.CreatedAt) || !loaded.UpdatedAt.Equal(saved.UpdatedAt) {
		t.Fatalf("transferred config fields changed: before=%+v after=%+v", saved, loaded)
	}
	if loaded.URL != saved.URL || loaded.Headers["Authorization"] != saved.Headers["Authorization"] {
		t.Fatalf("transferred secrets changed: %+v", loaded)
	}
	if _, ok, err := store.Get(ctx, ScopeUser, "loser", "home"); err != nil || ok {
		t.Fatalf("loser config still visible ok=%v err=%v", ok, err)
	}
}

func TestStoreMergeUsersTxRejectsConflictWithoutChanges(t *testing.T) {
	store := testStore(t)
	addTestUsers(t, store, "winner", "loser")
	ctx := context.Background()
	for _, owner := range []string{"winner", "loser"} {
		if _, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: owner, Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/" + owner}); err != nil {
			t.Fatalf("save %s config: %v", owner, err)
		}
	}

	tx, err := store.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin merge: %v", err)
	}
	if err := store.MergeUsersTx(ctx, tx, "winner", "loser"); err == nil {
		tx.Rollback() // nolint:errcheck
		t.Fatal("expected name conflict")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback merge: %v", err)
	}
	for _, owner := range []string{"winner", "loser"} {
		if _, ok, err := store.Get(ctx, ScopeUser, owner, "home"); err != nil || !ok {
			t.Fatalf("%s config changed after conflict ok=%v err=%v", owner, ok, err)
		}
	}
}

func TestStoreMergeUsersTxRejectsBadCiphertext(t *testing.T) {
	store := testStore(t)
	addTestUsers(t, store, "winner", "loser")
	ctx := context.Background()
	if _, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: "loser", Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/home"}); err != nil {
		t.Fatalf("save loser config: %v", err)
	}
	if _, err := store.db.SQL().Exec(`UPDATE mcp_servers SET url_ciphertext = 'invalid' WHERE owner_user_id = 'loser'`); err != nil {
		t.Fatalf("corrupt ciphertext: %v", err)
	}

	tx, err := store.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin merge: %v", err)
	}
	if err := store.MergeUsersTx(ctx, tx, "winner", "loser"); err == nil {
		tx.Rollback() // nolint:errcheck
		t.Fatal("expected bad ciphertext error")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback merge: %v", err)
	}
	var owner string
	if err := store.db.SQL().QueryRow(`SELECT owner_user_id FROM mcp_servers WHERE name = 'home'`).Scan(&owner); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if owner != "loser" {
		t.Fatalf("owner = %q, want loser", owner)
	}
}

func TestStoreSaveRejectsNonexistentOwner(t *testing.T) {
	store := testStore(t)
	_, err := store.Save(context.Background(), ServerConfig{Scope: ScopeUser, OwnerUserID: "missing", Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/home"})
	if err == nil {
		t.Fatal("expected nonexistent owner error")
	}
	var count int
	if queryErr := store.db.SQL().QueryRow(`SELECT COUNT(*) FROM mcp_servers`).Scan(&count); queryErr != nil {
		t.Fatalf("count configs: %v", queryErr)
	}
	if count != 0 {
		t.Fatalf("saved config count = %d, want 0", count)
	}
}
