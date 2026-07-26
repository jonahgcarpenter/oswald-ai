package database

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

func TestPublishedV3ResetPreservesIdentityAndMCPAndClearsState(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition string
	}{
		{name: "fresh-v3.2", definition: publishedV32SchemaDefinition},
		{name: "v3.1.2-upgraded", definition: strings.Replace(publishedV32SchemaDefinition, "'environment', 'notes'", "'environment', 'tasks', 'notes'", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oswald.db")
			ciphertext := []byte{0, 1, 2, 0xfe, 0xff}
			raw := createPublishedV3Database(t, path, test.definition)
			if _, err := raw.Exec(`
INSERT INTO account_users VALUES ('user-1', 'created-exact', 'updated-exact', 1, 1, 'banned-exact', 'admin-1', 'reason-exact');
INSERT INTO linked_accounts VALUES ('discord', 'discord-1', 'user-1', 'Discord Name', 'linked-exact', 1);
INSERT INTO user_memory_profiles VALUES ('user-1', 'old intro', 'created', 'updated');
INSERT INTO memory_entries(canonical_user_id, scope, category, statement, statement_key, evidence, source_session_id, created_at, updated_at)
VALUES ('user-1', 'long_term', 'identity', 'old memory', 'old-memory', 'evidence', 'session-1', 'created', 'updated');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, created_at) VALUES ('session-1', 'user-1', 'user', 'assistant', 'created');
INSERT INTO memory_events(memory_id, event_type, created_at) VALUES (1, 'saved', 'created');
INSERT INTO mcp_servers(id, scope, owner_user_id, name, transport, url_ciphertext, headers_ciphertext, created_at, updated_at)
VALUES ('mcp-1', 'user', 'user-1', 'server', 'streamable_http', ?, ?, 'mcp-created', 'mcp-updated');`, ciphertext, ciphertext); err != nil {
				raw.Close() // nolint:errcheck
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			db, err := Open(path, nil)
			if err != nil {
				t.Fatalf("open published v3 database: %v", err)
			}
			defer db.Close()
			var created, updated, bannedAt, bannedBy, reason, lifecycle, intro string
			var admin, banned int
			if err := db.SQL().QueryRow(`SELECT created_at, updated_at, is_admin, is_banned, banned_at, banned_by, ban_reason, lifecycle_state, speaker_intro FROM account_users WHERE canonical_user_id = 'user-1'`).Scan(&created, &updated, &admin, &banned, &bannedAt, &bannedBy, &reason, &lifecycle, &intro); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(created, "|", updated, "|", admin, "|", banned, "|", bannedAt, "|", bannedBy, "|", reason, "|", lifecycle, "|", intro); got != "created-exact|updated-exact|1|1|banned-exact|admin-1|reason-exact|active|You are speaking with Discord Name." {
				t.Fatalf("preserved account changed: %s", got)
			}
			var gateway, identifier, owner, displayName, linkedAt string
			var verified int
			if err := db.SQL().QueryRow(`SELECT gateway, identifier, canonical_user_id, display_name, linked_at, verified FROM linked_accounts`).Scan(&gateway, &identifier, &owner, &displayName, &linkedAt, &verified); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(gateway, "|", identifier, "|", owner, "|", displayName, "|", linkedAt, "|", verified); got != "discord|discord-1|user-1|Discord Name|linked-exact|1" {
				t.Fatalf("preserved link changed: %s", got)
			}
			var gotURL, gotHeaders []byte
			if err := db.SQL().QueryRow(`SELECT url_ciphertext, headers_ciphertext FROM mcp_servers WHERE id = 'mcp-1'`).Scan(&gotURL, &gotHeaders); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotURL, ciphertext) || !bytes.Equal(gotHeaders, ciphertext) {
				t.Fatalf("MCP ciphertext changed: url=%v headers=%v", gotURL, gotHeaders)
			}
			for _, table := range []string{"memory_entries", "memory_candidates", "session_turns", "session_summaries", "memory_events", "durable_jobs", "derived_index_revisions", "websocket_clients", "websocket_device_authorizations", "websocket_bootstrap_state", "global_memories", "sessions"} {
				var count int
				if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
					t.Fatalf("count reset table %s: %v", table, err)
				}
				if count != 0 {
					t.Fatalf("reset table %s retained %d rows", table, count)
				}
			}
			assertCompactV4LedgerAndIntegrity(t, db.SQL())
		})
	}
}

func TestPublishedV3ResetAcceptsAndDropsExactVectorObjectFamily(t *testing.T) {
	sqlite_vec.Auto()
	for _, test := range []struct {
		name       string
		definition string
	}{
		{name: "fresh-v3.2", definition: publishedV32SchemaDefinition},
		{name: "v3.1.2-upgraded", definition: strings.Replace(publishedV32SchemaDefinition, "'environment', 'notes'", "'environment', 'tasks', 'notes'", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oswald.db")
			raw := createPublishedV3Database(t, path, test.definition)
			vector, err := sqlite_vec.SerializeFloat32([]float32{1, 2, 3})
			if err != nil {
				raw.Close() // nolint:errcheck
				t.Fatal(err)
			}
			if _, err := raw.Exec(`
INSERT INTO account_users VALUES ('user', 'created', 'updated', 0, 0, '', '', '');
INSERT INTO memory_entries(canonical_user_id, scope, category, statement, statement_key, evidence, created_at, updated_at, embedding_model, embedding_dim)
VALUES ('user', 'long_term', 'identity', 'vector memory', 'vector memory', 'evidence', 'created', 'updated', 'published-model', 3);
CREATE VIRTUAL TABLE memory_entry_vectors USING vec0(embedding float[3]);
INSERT INTO memory_entry_vectors(rowid, embedding) VALUES (1, ?);`, vector); err != nil {
				raw.Close() // nolint:errcheck
				t.Fatalf("create vector-enabled published v3 fixture: %v", err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			db, err := Open(path, nil)
			if err != nil {
				t.Fatalf("open vector-enabled published v3 database: %v", err)
			}
			defer db.Close()
			var vectorObjects int
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'memory_entry_vectors' OR name GLOB 'memory_entry_vectors_*'`).Scan(&vectorObjects); err != nil {
				t.Fatal(err)
			}
			if vectorObjects != 0 {
				t.Fatalf("selective reset retained %d published vector objects", vectorObjects)
			}
			assertCompactV4LedgerAndIntegrity(t, db.SQL())
		})
	}
}

func TestPublishedV3ResetReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw := createPublishedV3Database(t, path, publishedV32SchemaDefinition)
	if _, err := raw.Exec(`INSERT INTO account_users VALUES ('user', 'created', 'updated', 0, 0, '', '', '')`); err != nil {
		t.Fatal(err)
	}
	raw.Close() // nolint:errcheck
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // nolint:errcheck
	db, err = Open(path, nil)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	defer db.Close()
	var users, ledger int
	if err := db.SQL().QueryRow(`SELECT (SELECT COUNT(*) FROM account_users), (SELECT COUNT(*) FROM schema_migration_versions)`).Scan(&users, &ledger); err != nil {
		t.Fatal(err)
	}
	if users != 1 || ledger != 1 {
		t.Fatalf("reopen changed migrated state: users=%d ledger=%d", users, ledger)
	}
}

func TestPublishedV3ResetRejectsMalformedOwnershipWithoutChangingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw := createPublishedV3Database(t, path, publishedV32SchemaDefinition)
	if _, err := raw.Exec(`PRAGMA ignore_check_constraints = ON; INSERT INTO mcp_servers(id, scope, owner_user_id, name, transport, url_ciphertext, created_at, updated_at) VALUES ('bad', 'user', 'missing', 'bad', 'sse', 'cipher', 'created', 'updated')`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	raw.Close() // nolint:errcheck
	if db, err := Open(path, nil); err == nil {
		db.Close() // nolint:errcheck
		t.Fatal("expected malformed MCP ownership rejection")
	} else if !strings.Contains(err.Error(), "ownership validation failed") {
		t.Fatalf("unexpected rejection: %v", err)
	}
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if after := schemaSnapshot(t, raw); after != before {
		t.Fatal("failed reset changed the original v3 schema")
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM mcp_servers WHERE id = 'bad' AND owner_user_id = 'missing'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed reset changed original v3 data: count=%d err=%v", count, err)
	}
}

func TestPublishedV3ResetConcurrentOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw := createPublishedV3Database(t, path, publishedV32SchemaDefinition)
	if _, err := raw.Exec(`INSERT INTO account_users VALUES ('user', 'created', 'updated', 0, 0, '', '', '')`); err != nil {
		t.Fatal(err)
	}
	raw.Close() // nolint:errcheck
	const count = 8
	dbs := make([]*DB, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range dbs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbs[i], errs[i] = Open(path, nil)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent migration open %d: %v", i, err)
		}
		defer dbs[i].Close()
	}
	assertCompactV4LedgerAndIntegrity(t, dbs[0].SQL())
}

func TestPublishedV3ResetRejectsUnknownStructuralVariant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	definition := publishedV32SchemaDefinition + `CREATE TABLE experimental_extra (id INTEGER PRIMARY KEY);`
	raw := createPublishedV3Database(t, path, definition)
	raw.Close() // nolint:errcheck
	if db, err := Open(path, nil); err == nil {
		db.Close() // nolint:errcheck
		t.Fatal("expected unknown v3-like schema rejection")
	} else if !strings.Contains(err.Error(), "predates the disposable v4 baseline") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestPublishedV3ResetRejectsUnknownObjectAlongsideExactVectorFamily(t *testing.T) {
	sqlite_vec.Auto()
	path := filepath.Join(t.TempDir(), "oswald.db")
	definition := publishedV32SchemaDefinition + `
CREATE VIRTUAL TABLE memory_entry_vectors USING vec0(embedding float[3]);
CREATE TABLE experimental_extra (id INTEGER PRIMARY KEY);`
	raw := createPublishedV3Database(t, path, definition)
	raw.Close() // nolint:errcheck
	if db, err := Open(path, nil); err == nil {
		db.Close() // nolint:errcheck
		t.Fatal("expected vector-enabled schema with an unknown object to be rejected")
	} else if !strings.Contains(err.Error(), "predates the disposable v4 baseline") {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestCompactV4MigrationChecksumCoversResetContractsAndImportSQL(t *testing.T) {
	migration := orderedMigrations()[0]
	for name, change := range map[string][2]string{
		"structural fingerprint contract": {publishedV3StructuralFingerprintContractVersion, "published-v3-structural-fingerprint-v2"},
		"speaker intro contract":          {publishedV3SpeakerIntroRenderingContractVersion, "published-v3-speaker-intro-v2"},
		"speaker intro rendering":         {publishedV3SpeakerIntroAliasFormat, "You are speaking with %s, also known as %s."},
		"account import mapping":          {"SELECT canonical_user_id, created_at, updated_at, is_admin", "SELECT canonical_user_id, updated_at, created_at, is_admin"},
		"vector drop SQL":                 {"DROP TABLE memory_entry_vectors;", "DROP TABLE IF EXISTS memory_entry_vectors;"},
	} {
		t.Run(name, func(t *testing.T) {
			changed := migration
			changed.definition = strings.Replace(changed.definition, change[0], change[1], 1)
			if changed.definition == migration.definition {
				t.Fatalf("contract %q is absent from checksum definition", change[0])
			}
			if migrationChecksum(changed) == migrationChecksum(migration) {
				t.Fatal("migration checksum did not change")
			}
		})
	}
}

func createPublishedV3Database(t *testing.T, path, definition string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(definition); err != nil {
		db.Close() // nolint:errcheck
		t.Fatalf("create frozen published v3 database: %v", err)
	}
	return db
}

func assertCompactV4LedgerAndIntegrity(t *testing.T, db *sql.DB) {
	t.Helper()
	var version int
	var name, checksum string
	if err := db.QueryRow(`SELECT version, name, checksum FROM schema_migration_versions`).Scan(&version, &name, &checksum); err != nil {
		t.Fatal(err)
	}
	if version != 1 || name != "v4_compact_baseline" || checksum != migrationChecksum(orderedMigrations()[0]) {
		t.Fatalf("unexpected compact v4 ledger: %d %q %q", version, name, checksum)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("migrated database has a foreign-key violation")
	}
}
