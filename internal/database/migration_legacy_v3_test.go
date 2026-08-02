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

func TestLegacyV320MigrationPreservesOnlyReducedIdentityAndMCP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw := createLegacyV320Database(t, path)
	ciphertext := []byte{0, 1, 2, 0xfe, 0xff}
	if _, err := raw.Exec(`
INSERT INTO account_users VALUES ('user-1', 'discard-created', 'discard-updated', 1, 1, 'discard-banned-at', 'discard-banned-by', 'reason-exact');
INSERT INTO account_users VALUES ('websocket-only', 'discard-created', 'discard-updated', 0, 0, '', '', '');
INSERT INTO linked_accounts VALUES ('imessage', 'imessage-1', 'user-1', 'Primary Name', 'discard-linked-at', 1);
INSERT INTO linked_accounts VALUES ('discord', 'discord-1', 'user-1', 'Alias Name', 'discard-linked-at', 0);
INSERT INTO linked_accounts VALUES ('websocket', 'remote:1234', 'user-1', 'Retired Link', 'discard-linked-at', 0);
INSERT INTO linked_accounts VALUES ('websocket', 'remote:5678', 'websocket-only', 'Retired Only', 'discard-linked-at', 0);
INSERT INTO user_memory_profiles VALUES ('user-1', 'discard-intro', 'created', 'updated');
INSERT INTO memory_entries(canonical_user_id, scope, category, statement, statement_key, evidence, source_session_id, created_at, updated_at)
VALUES ('user-1', 'long_term', 'tasks', 'discard-memory', 'discard-memory', 'evidence', 'session', 'created', 'updated');
INSERT INTO session_turns(session_id, canonical_user_id, user_text, assistant_text, topic_tags, created_at)
VALUES ('session', 'user-1', 'discard-user', 'discard-assistant', 'tag', 'created');
INSERT INTO memory_events(memory_id, event_type, created_at) VALUES (1, 'saved', 'created');
INSERT INTO mcp_servers(id, scope, owner_user_id, name, type, transport, url_ciphertext, url_host_hash, headers_ciphertext, enabled, created_at, updated_at)
VALUES ('mcp-1', 'user', 'user-1', 'server', 'discard-type', 'streamable_http', ?, 'discard-host-hash', ?, 0, 'discard-created', 'discard-updated');`, ciphertext, ciphertext); err != nil {
		t.Fatal(err)
	}
	raw.Close() // nolint:errcheck

	db, err := Open(path, nil)
	if err != nil {
		t.Fatalf("migrate tagged v3.2.0 database: %v", err)
	}
	defer db.Close()
	var admin, banned int
	var reason, lifecycle, intro string
	if err := db.SQL().QueryRow(`SELECT is_admin, is_banned, ban_reason, lifecycle_state, speaker_intro FROM account_users WHERE canonical_user_id = 'user-1'`).Scan(&admin, &banned, &reason, &lifecycle, &intro); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%d|%d|%s|%s|%s", admin, banned, reason, lifecycle, intro); got != "1|1|reason-exact|active|You are speaking with Primary Name aka Alias Name." {
		t.Fatalf("unexpected migrated user: %s", got)
	}
	var gateway, identifier, owner, display string
	var verified int
	if err := db.SQL().QueryRow(`SELECT gateway, identifier, canonical_user_id, display_name, verified FROM linked_accounts WHERE gateway = 'discord'`).Scan(&gateway, &identifier, &owner, &display, &verified); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%s|%s|%s|%s|%d", gateway, identifier, owner, display, verified); got != "discord|discord-1|user-1|Alias Name|0" {
		t.Fatalf("unexpected migrated linked account: %s", got)
	}
	var gotURL, gotHeaders []byte
	var scope, mcpOwner, name, transport string
	var enabled int
	if err := db.SQL().QueryRow(`SELECT scope, owner_user_id, name, transport, url_ciphertext, headers_ciphertext, enabled FROM mcp_servers`).Scan(&scope, &mcpOwner, &name, &transport, &gotURL, &gotHeaders, &enabled); err != nil {
		t.Fatal(err)
	}
	if scope != "user" || mcpOwner != "user-1" || name != "server" || transport != "streamable_http" || enabled != 0 || !bytes.Equal(gotURL, ciphertext) || !bytes.Equal(gotHeaders, ciphertext) {
		t.Fatalf("unexpected migrated MCP server: %q %q %q %q %d %v %v", scope, mcpOwner, name, transport, enabled, gotURL, gotHeaders)
	}
	var websocketLinks, websocketOnlyUsers int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM linked_accounts WHERE gateway = 'websocket'`).Scan(&websocketLinks); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM account_users WHERE canonical_user_id = 'websocket-only' AND speaker_intro = 'You are speaking with a returning user.'`).Scan(&websocketOnlyUsers); err != nil {
		t.Fatal(err)
	}
	if websocketLinks != 0 || websocketOnlyUsers != 1 {
		t.Fatalf("retired websocket cleanup: links=%d preserved_users=%d", websocketLinks, websocketOnlyUsers)
	}
	for _, table := range []string{"memory_entries", "memory_candidates", "session_turns", "session_summaries", "durable_jobs", "derived_index_revisions", "global_memories", "sessions"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("legacy state survived in %s", table)
		}
	}
	assertPermanentV4Ledger(t, db.SQL())
}

func TestLegacyV320MigrationAcceptsExactOptionalVectorFamily(t *testing.T) {
	sqlite_vec.Auto()
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw := createLegacyV320Database(t, path)
	vector, err := sqlite_vec.SerializeFloat32([]float32{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
INSERT INTO account_users VALUES ('user', 'created', 'updated', 0, 0, '', '', '');
INSERT INTO memory_entries(canonical_user_id, scope, category, statement, statement_key, evidence, created_at, updated_at)
VALUES ('user', 'long_term', 'identity', 'discard', 'discard', 'evidence', 'created', 'updated');
CREATE VIRTUAL TABLE memory_entry_vectors USING vec0(embedding float[3]);
INSERT INTO memory_entry_vectors(rowid, embedding) VALUES (1, ?);`, vector); err != nil {
		t.Fatal(err)
	}
	raw.Close() // nolint:errcheck
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'memory_entry_vectors' OR name GLOB 'memory_entry_vectors_*'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy vector family retained %d objects", count)
	}
}

func TestLegacyV320MigrationRebuildsSpeakerIntrosWithRuntimeSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw := createLegacyV320Database(t, path)
	if _, err := raw.Exec(`
INSERT INTO account_users VALUES ('unicode', 'created', 'updated', 0, 0, '', '', '');
INSERT INTO account_users VALUES ('home', 'created', 'updated', 0, 0, '', '', '');
INSERT INTO account_users VALUES ('fallback', 'created', 'updated', 0, 0, '', '', '');
INSERT INTO linked_accounts VALUES ('imessage', 'unicode-imessage', 'unicode', 'Élodie', 'linked', 1);
INSERT INTO linked_accounts VALUES ('discord', 'unicode-discord', 'unicode', 'élodie', 'linked', 1);
INSERT INTO linked_accounts VALUES ('homeassistant', 'home-id', 'home', 'Home Name', 'linked', 1);
INSERT INTO linked_accounts VALUES ('discord', 'blank-id', 'fallback', '   ', 'linked', 1);`); err != nil {
		t.Fatal(err)
	}
	raw.Close() // nolint:errcheck
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for userID, want := range map[string]string{
		"unicode":  "You are speaking with Élodie.",
		"home":     "You are speaking with Home Name.",
		"fallback": "You are speaking with a returning user.",
	} {
		var got string
		if err := db.SQL().QueryRow(`SELECT speaker_intro FROM account_users WHERE canonical_user_id = ?`, userID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("speaker intro for %s = %q, want %q", userID, got, want)
		}
	}
}

func TestLegacyV320MigrationRejectsUnknownObjectWithVectorFamily(t *testing.T) {
	sqlite_vec.Auto()
	path := filepath.Join(t.TempDir(), "oswald.db")
	raw := createLegacyV320Database(t, path)
	if _, err := raw.Exec(`CREATE VIRTUAL TABLE memory_entry_vectors USING vec0(embedding float[7]); CREATE TABLE experimental_extra (id INTEGER PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	raw.Close() // nolint:errcheck
	if db, err := Open(path, nil); err == nil {
		db.Close() // nolint:errcheck
		t.Fatal("expected vector-enabled unknown schema rejection")
	}
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if after := schemaSnapshot(t, raw); after != before {
		t.Fatal("rejected vector-enabled schema was changed")
	}
}

func TestLegacyV320MigrationRejectsInaccessibleWebsocketOwnershipAndUnknownWithoutChange(t *testing.T) {
	for _, test := range []struct {
		name      string
		setup     string
		wantError string
	}{
		{name: "websocket-only administrator", setup: `INSERT INTO account_users VALUES ('user', 'created', 'updated', 1, 0, '', '', ''); INSERT INTO linked_accounts VALUES ('websocket', 'legacy:1', 'user', 'Legacy', 'linked', 1);`, wantError: "websocket-only privileged ownership validation failed"},
		{name: "websocket-only MCP owner", setup: `INSERT INTO account_users VALUES ('user', 'created', 'updated', 0, 0, '', '', ''); INSERT INTO linked_accounts VALUES ('websocket', 'legacy:2', 'user', 'Legacy', 'linked', 1); INSERT INTO mcp_servers(id, scope, owner_user_id, name, transport, url_ciphertext, created_at, updated_at) VALUES ('owned', 'user', 'user', 'owned', 'streamable_http', 'cipher', 'created', 'updated');`, wantError: "websocket-only privileged ownership validation failed"},
		{name: "invalid owner", setup: `INSERT INTO mcp_servers(id, scope, owner_user_id, name, transport, url_ciphertext, created_at, updated_at) VALUES ('bad', 'user', 'missing', 'bad', 'sse', 'cipher', 'created', 'updated');`, wantError: "ownership validation failed"},
		{name: "unknown schema", setup: `CREATE TABLE experimental_extra (id INTEGER PRIMARY KEY);`, wantError: "no recognized permanent migration ledger or exact v3.2.0 schema"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oswald.db")
			raw := createLegacyV320Database(t, path)
			if _, err := raw.Exec(test.setup); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, raw)
			raw.Close() // nolint:errcheck
			if db, err := Open(path, nil); err == nil {
				db.Close() // nolint:errcheck
				t.Fatal("expected migration rejection")
			} else if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("unexpected error: %v", err)
			}
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if after := schemaSnapshot(t, raw); after != before {
				t.Fatal("rejected migration changed source schema")
			}
		})
	}
}

func TestLegacyV320ConcurrentMigrationAndFreshSchemaMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw := createLegacyV320Database(t, path)
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
			t.Fatalf("concurrent migration %d: %v", i, err)
		}
		defer dbs[i].Close()
	}
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if migrated, clean := schemaSnapshot(t, dbs[0].SQL()), schemaSnapshot(t, fresh.SQL()); migrated != clean {
		t.Fatal("migrated and fresh databases have different canonical schemas")
	}
	assertPermanentV4Ledger(t, dbs[0].SQL())
}

func createLegacyV320Database(t *testing.T, path string) *sql.DB {
	t.Helper()
	definition, err := legacyMigrationFiles.ReadFile("legacy_migrations/v3.2.0_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(definition)); err != nil {
		db.Close() // nolint:errcheck
		t.Fatal(err)
	}
	return db
}

func assertPermanentV4Ledger(t *testing.T, db *sql.DB) {
	t.Helper()
	var version, count int
	var name, checksum string
	if err := db.QueryRow(`SELECT version, name, checksum FROM schema_migration_versions`).Scan(&version, &name, &checksum); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migration_versions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if version != 1 || count != 1 || name != "v4.0.0" || checksum != migrationChecksum(orderedMigrations()[0]) {
		t.Fatalf("unexpected permanent ledger: version=%d count=%d name=%q checksum=%q", version, count, name, checksum)
	}
}
