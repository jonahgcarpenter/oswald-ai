package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAPIKeyMigrationPreservesLinksAndRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-keys.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	db := &DB{path: path, db: raw}
	registry := orderedMigrations()
	if len(registry) < 15 || registry[14].name != "v4.0.14" {
		t.Fatal("unexpected API key migration position")
	}
	ctx := context.Background()
	if err := db.runSchemaMigrations(ctx, registry[:14]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('owner'), ('other');
		INSERT INTO linked_accounts(gateway, identifier, canonical_user_id, display_name, verified)
		VALUES ('discord', '123', 'owner', 'Alice', 1), ('discord', '456', 'other', 'Bob', 0)`); err != nil {
		t.Fatal(err)
	}
	broken := append([]schemaMigration(nil), registry...)
	broken[14].sql += "\ninvalid statement;"
	if err := db.runSchemaMigrations(ctx, broken); err == nil {
		t.Fatal("broken migration succeeded")
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM linked_accounts`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rollback lost links: count=%d err=%v", count, err)
	}
	if err := db.runSchemaMigrations(ctx, registry); err != nil {
		t.Fatal(err)
	}
	var name string
	var verified int
	if err := raw.QueryRow(`SELECT display_name, verified FROM linked_accounts WHERE identifier = '123'`).Scan(&name, &verified); err != nil || name != "Alice" || verified != 1 {
		t.Fatalf("migration changed existing link: name=%q verified=%d err=%v", name, verified, err)
	}
	if _, err := raw.Exec(`INSERT INTO linked_accounts(gateway, identifier, canonical_user_id) VALUES ('discord', '789', 'owner')`); err == nil {
		t.Fatal("duplicate non-API gateway accepted")
	}
	for _, id := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		if _, err := raw.Exec(`INSERT INTO linked_accounts(gateway, identifier, canonical_user_id) VALUES ('openai', ?, 'owner')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`INSERT INTO api_keys(key_id, canonical_user_id, secret_hash) VALUES (?, 'owner', zeroblob(32))`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO api_keys(key_id, canonical_user_id, secret_hash) VALUES ('short', 'owner', zeroblob(31))`); err == nil {
		t.Fatal("invalid hash size accepted")
	}
	if _, err := raw.Exec(`DELETE FROM account_users WHERE canonical_user_id = 'owner'`); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted owner retained keys: count=%d err=%v", count, err)
	}
	if err := db.runSchemaMigrations(ctx, registry); err != nil {
		t.Fatalf("migration not idempotent: %v", err)
	}
}
