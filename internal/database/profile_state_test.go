package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestProfileStateFreshReopenFTSAndForeignKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenState(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO sessions(id,source,started_at,profile_name) VALUES ('session','discord',1,'alice'); INSERT INTO messages(session_id,role,content,timestamp) VALUES ('session','user','a blue elephant',1)`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages_fts", "messages_fts_trigram"} {
		var count int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE ` + table + ` MATCH 'elephant'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if _, err := db.SQL().Exec(`INSERT INTO messages(session_id,role,timestamp) VALUES ('missing','user',1)`); err == nil {
		t.Fatal("foreign key not enforced")
	}
	if _, err := db.SQL().Exec(`UPDATE messages SET content='a green giraffe'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`DELETE FROM messages`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenState(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sessions int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatal("reopen lost data", err)
	}
}

func TestProfileStateRejectsExistingSchemaWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE operator_data(value TEXT); INSERT INTO operator_data VALUES ('preserve')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := OpenState(ctx, path, nil); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("error=%v", err)
	}
	raw, err = sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var value string
	if err := raw.QueryRow(`SELECT value FROM operator_data`).Scan(&value); err != nil || value != "preserve" {
		t.Fatal("existing data changed", err)
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name='schema_version'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("existing schema changed", err)
	}
}

func TestProfileStateRejectsSchemaDrift(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenState(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`DROP TRIGGER messages_fts_insert`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := OpenState(ctx, path, nil); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("error=%v", err)
	}
}

func TestProfileStateAcceptsCanonicalSchemaWithDifferentFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	// A .schema dump uses different whitespace and may include comments. It is
	// still the approved shape; version rows are checked separately.
	formatted := strings.ReplaceAll(profileSchema, "(", "(\n    ")
	formatted = strings.ReplaceAll(formatted, "CREATE TABLE sessions", "CREATE TABLE /* dump annotation */ sessions")
	if _, err := raw.Exec(formatted); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version VALUES (?)`, ProfileSchemaVersion); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()
	db, err := OpenState(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
}

func TestSchemaSQLNormalizationPreservesQuotedValues(t *testing.T) {
	left := normalizeSchemaSQL(`CREATE TABLE t(value TEXT DEFAULT 'two  spaces -- not a comment');`)
	right := normalizeSchemaSQL("create table t ( value TEXT /* comment */ DEFAULT 'two  spaces -- not a comment' ) ;")
	if left != right {
		t.Fatal("formatting changed schema identity")
	}
	if left == normalizeSchemaSQL(`CREATE TABLE t(value TEXT DEFAULT 'two spaces -- not a comment');`) {
		t.Fatal("quoted value change was hidden")
	}
}

func TestProfileStateRejectsVersionChangesWithoutRepair(t *testing.T) {
	for _, mutation := range []string{`DELETE FROM schema_version`, `UPDATE schema_version SET version=2`, `INSERT INTO schema_version VALUES(1)`} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenState(context.Background(), path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().Exec(mutation); err != nil {
				t.Fatal(err)
			}
			var before int
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			db.Close()
			if _, err := OpenState(context.Background(), path, nil); !errors.Is(err, ErrIncompatibleSchema) {
				t.Fatal("version mutation accepted", err)
			}
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			var after int
			if err := raw.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&after); err != nil || after != before {
				t.Fatal("version rows were repaired", err)
			}
		})
	}
}

func TestProfileStateRefusesSymlinksAndPreCanceledInitialization(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(context.Background(), filepath.Join(link, "state.db"), nil); err == nil {
		t.Fatal("symlinked ancestor accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "state.db")); !os.IsNotExist(err) {
		t.Fatal("unsafe open created external database")
	}
	path := filepath.Join(root, "state.db")
	if err := os.Symlink(filepath.Join(outside, "sidecar"), path+"-wal"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(context.Background(), path, nil); err == nil {
		t.Fatal("symlinked SQLite sidecar accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledPath := filepath.Join(root, "canceled.db")
	if _, err := OpenState(ctx, canceledPath, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-canceled open was not canceled", err)
	}
	if _, err := os.Stat(canceledPath); !os.IsNotExist(err) {
		t.Fatal("pre-canceled open created database")
	}
}

func TestProfileStateApplicationTableInventory(t *testing.T) {
	db, err := OpenState(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.SQL().Query(`SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	approved := map[string]bool{}
	for _, name := range []string{"schema_version", "system_prompts", "sessions", "messages", "session_model_usage", "state_meta", "gateway_routing", "gateway_hygiene_state", "conversation_generations", "gateway_heartbeats", "compression_locks", "session_turn_leases", "async_delegations", "sqlite_sequence", "messages_fts", "messages_fts_trigram"} {
		approved[name] = true
	}
	for _, prefix := range []string{"messages_fts", "messages_fts_trigram"} {
		for _, suffix := range []string{"_data", "_idx", "_docsize", "_config"} {
			approved[prefix+suffix] = true
		}
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !approved[name] {
			t.Fatal("unapproved application table", name)
		}
		delete(approved, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(approved) != 0 {
		t.Fatal("approved table missing", approved)
	}
}

func TestProfileStateConcurrentOpenAndProfileIsolation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := OpenState(ctx, path, nil)
			if err == nil {
				err = db.Close()
			}
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	alice, err := OpenState(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := OpenState(ctx, filepath.Join(root, "bob.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	if _, err := alice.SQL().Exec(`INSERT INTO sessions(id,source,started_at) VALUES ('same-conversation','discord',1)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := bob.SQL().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("cross-profile state leaked", err)
	}
}

func TestProfileStateInitializationLogsAreSafeAtInfoAndDebug(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var output bytes.Buffer
			log := config.NewLogger(level)
			log.SetOutput(&output)
			path := filepath.Join(t.TempDir(), "private-path-canary.db")
			db, err := OpenState(context.Background(), path, log)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			if strings.Contains(output.String(), "private-path-canary") || strings.Count(output.String(), `"event":"database.schema.complete"`) != 1 || !strings.Contains(output.String(), `"is_created":true`) {
				t.Fatal("unsafe or missing schema measurement")
			}
		})
	}
}

func TestProfileStateDisplayOrderTriggersAndFTSFilters(t *testing.T) {
	db, err := OpenState(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SQL().Exec(`INSERT INTO sessions(id,source,started_at) VALUES ('ordinary','discord',1),('background','cron',1);
 INSERT INTO messages(session_id,role,content,timestamp,display_identity) VALUES ('ordinary','user','test elephant',1,X'01'),('ordinary','assistant','same display group',2,X'01'),('background','user','background elephant',1,NULL),('ordinary','tool','tool elephant',3,NULL);`); err != nil {
		t.Fatal(err)
	}
	var first, second int64
	if err := db.SQL().QueryRow(`SELECT display_order FROM messages WHERE id=1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT display_order FROM messages WHERE id=2`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("display identities were not grouped")
	}
	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM messages_fts_trigram WHERE messages_fts_trigram MATCH 'elephant'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("trigram filtering failed", err)
	}
	if _, err := db.SQL().Exec(`UPDATE messages SET content='changed display identity' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM messages WHERE id IN (1,2) AND display_identity IS NULL`).Scan(&count); err != nil || count != 2 {
		t.Fatal("display identity invalidation failed", err)
	}
}
