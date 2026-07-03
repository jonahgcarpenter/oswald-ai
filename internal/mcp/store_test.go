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

func TestStoreEncryptsURLAndHeadersAtRest(t *testing.T) {
	store := testStore(t)
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
