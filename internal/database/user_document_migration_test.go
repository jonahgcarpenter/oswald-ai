package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestUserDocumentMigrationUpgradeRollbackAndCascade(t *testing.T) {
	raw, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "documents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	db := &DB{db: raw}
	ctx := context.Background()
	registry := orderedMigrations()
	if len(registry) != 15 || registry[14].name != "v4.0.14" {
		t.Fatal("unexpected migration registry")
	}
	if err = db.runSchemaMigrations(ctx, registry[:14]); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`INSERT INTO account_users(canonical_user_id) VALUES('user')`); err != nil {
		t.Fatal(err)
	}
	before := schemaSnapshot(t, raw)
	broken := orderedMigrations()
	broken[14].sql += "\ninvalid statement;"
	if err = db.runSchemaMigrations(ctx, broken); err == nil {
		t.Fatal("broken migration committed")
	}
	if after := schemaSnapshot(t, raw); before != after {
		t.Fatal("schema changed after rollback")
	}
	if err = db.runSchemaMigrations(ctx, registry); err != nil {
		t.Fatal(err)
	}
	if err = db.runSchemaMigrations(ctx, registry); err != nil {
		t.Fatal("reopen", err)
	}
	if _, err = raw.Exec(`INSERT INTO user_documents(id,canonical_user_id,filename,media_type,source_hash,status,source_bytes,accepted_at,expires_at) VALUES('opaque','user','test.txt','text/plain','hash','queued',1,1,2);
INSERT INTO user_document_sources(document_id,data) VALUES('opaque',X'61');
INSERT INTO user_document_chunks(document_id,ordinal,text,locator,method) VALUES('opaque',0,'text','','');
INSERT INTO user_document_extraction_jobs(document_id,state) VALUES('opaque','queued');
INSERT INTO user_document_reservations(id,canonical_user_id,file_count,source_bytes,reserved_text_bytes,created_at,expires_at) VALUES('reservation','user',1,1,1048576,1,300001);`); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`INSERT INTO user_document_sources(document_id,data) VALUES('missing',X'61')`); err == nil {
		t.Fatal("orphan source accepted")
	}
	if _, err = raw.Exec(`UPDATE user_documents SET source_bytes=20971521`); err == nil {
		t.Fatal("oversized source metadata accepted")
	}
	if _, err = raw.Exec(`UPDATE user_document_reservations SET expires_at=300002`); err == nil {
		t.Fatal("renewable reservation accepted")
	}
	if _, err = raw.Exec(`DELETE FROM account_users WHERE canonical_user_id='user'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"user_documents", "user_document_sources", "user_document_chunks", "user_document_extraction_jobs", "user_document_reservations"} {
		var count int
		if err = raw.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cascade %s: %d %v", table, count, err)
		}
	}
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = foreignKeyCheck(ctx, conn); err != nil {
		t.Fatal(err)
	}
}
