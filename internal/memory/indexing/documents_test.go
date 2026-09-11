package indexing

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

func TestDocumentIndexWorkerBuildOutboxMergeAndReplacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "documents.db")
	s := newLifecycleStoreAt(t, path, "user", "winner")
	publish := func(text string) memory.UserDocument {
		docs, err := s.AcceptUserDocuments(ctx, "user", []memory.DocumentUpload{{Filename: "test.txt", MediaType: "text/plain", Data: []byte(text)}})
		if err != nil {
			t.Fatal(err)
		}
		job, err := s.ClaimUserDocumentExtraction(ctx, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CompleteUserDocumentExtraction(ctx, job, []memory.DocumentChunk{{Ordinal: 0, Text: text, Method: "text"}}, false); err != nil {
			t.Fatal(err)
		}
		return docs[0]
	}
	d := publish("First synthetic document.")
	e := &lifecycleEmbedder{dimensions: map[string]int{"old": 2, "new": 3}}
	worker := NewService(s, nil, e, "old", nil)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{memory.IndexKindUserDocumentFTS, memory.IndexKindUserDocumentVector} {
		r, err := s.LiveIndexRevision(ctx, kind)
		if err != nil || r.IndexedCount != 1 {
			t.Fatalf("build %s: %+v %v", kind, r, err)
		}
	}
	publish("Second document after initial revision.")
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check := func(owner string, want int) {
		for _, kind := range []string{memory.IndexKindUserDocumentFTS, memory.IndexKindUserDocumentVector} {
			r, err := s.LiveIndexRevision(ctx, kind)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM `+r.TableName+` WHERE canonical_user_id=?`, owner).Scan(&n); err != nil || n != want {
				t.Fatalf("%s owner %s count=%d want=%d err=%v", kind, owner, n, want, err)
			}
		}
	}
	check("user", 2)
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.MergeUsersTx(ctx, tx, "winner", "user", ""); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	check("user", 0)
	check("winner", 2)
	worker = NewService(s, nil, e, "new", nil)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := s.LiveIndexRevision(ctx, memory.IndexKindUserDocumentVector)
	if err != nil || r.Model != "new" || r.Dimension != 3 || r.IndexedCount != 2 {
		t.Fatalf("replacement %+v %v", r, err)
	}
	if _, err := s.DeleteUserDocuments(ctx, memory.DocumentScope{UserID: "winner"}, []string{d.ID}); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	check("winner", 1)
	if _, err := s.MaintainDerivedIndexes(ctx, time.Now(), time.Hour, 100); err != nil {
		t.Fatal(err)
	}
}
