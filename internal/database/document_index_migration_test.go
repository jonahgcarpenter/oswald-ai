package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentIndexMigrationPreservesTriggersAndSequences(t *testing.T) {
	raw, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	db := &DB{db: raw}
	ctx := context.Background()
	registry := orderedMigrations()
	if err = db.runSchemaMigrations(ctx, registry[:14]); err != nil {
		t.Fatal(err)
	}
	triggers := func() map[string]string {
		rows, err := raw.Query(`SELECT name,sql FROM sqlite_master WHERE type='trigger'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, ddl string
			if err := rows.Scan(&name, &ddl); err != nil {
				t.Fatal(err)
			}
			out[name] = strings.ToLower(strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(ddl))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := triggers()
	if _, err = raw.Exec(`UPDATE sqlite_sequence SET seq=9876 WHERE name='durable_jobs'; UPDATE sqlite_sequence SET seq=8765 WHERE name='derived_index_revisions'; INSERT INTO sqlite_sequence(name,seq) SELECT 'durable_jobs',9876 WHERE NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='durable_jobs'); INSERT INTO sqlite_sequence(name,seq) SELECT 'derived_index_revisions',8765 WHERE NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='derived_index_revisions');`); err != nil {
		t.Fatal(err)
	}
	if err = db.runSchemaMigrations(ctx, registry); err != nil {
		t.Fatal(err)
	}
	after := triggers()
	for name, ddl := range before {
		if after[name] != ddl {
			t.Errorf("changed trigger %s\nbefore %s\nafter %s", name, ddl, after[name])
		}
	}
	for table, want := range map[string]int{"durable_jobs": 9876, "derived_index_revisions": 8765} {
		var got int
		if err := raw.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name=?`, table).Scan(&got); err != nil || got != want {
			t.Errorf("sequence %s: %d %v", table, got, err)
		}
	}
}
