package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func documentUpload(key string) DocumentUpload {
	return DocumentUpload{Filename: "synthetic.txt", MediaType: "text/plain", AdmissionKey: key, Data: []byte("synthetic source")}
}

func acceptDocument(t *testing.T, s *Store, user string) UserDocument {
	t.Helper()
	docs, err := s.AcceptUserDocuments(context.Background(), user, []DocumentUpload{documentUpload("")})
	if err != nil {
		t.Fatal(err)
	}
	return docs[0]
}

func claimDocument(t *testing.T, s *Store) *DocumentExtractionJob {
	t.Helper()
	j, err := s.ClaimUserDocumentExtraction(context.Background(), "worker")
	if err != nil || j == nil {
		t.Fatalf("claim: %v, %v", j, err)
	}
	return j
}

func TestUserDocumentsIsolationAdmissionAndPaging(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "other")
	ctx := context.Background()
	if _, err := s.AcceptUserDocuments(ctx, "missing", []DocumentUpload{documentUpload("")}); err == nil {
		t.Fatal("missing owner accepted")
	}
	u := documentUpload("retry")
	docs, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{u})
	if err != nil {
		t.Fatal(err)
	}
	d := docs[0]
	if len(d.ID) != 32 || d.ExpiresAt.Sub(d.AcceptedAt) != DocumentLifetime {
		t.Fatalf("metadata: %+v", d)
	}
	again, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{u})
	if err != nil || again[0].ID != d.ID {
		t.Fatalf("replay: %v", err)
	}
	u.Data = []byte("different")
	if _, err = s.AcceptUserDocuments(ctx, "user", []DocumentUpload{documentUpload("new"), u}); !errors.Is(err, ErrDocumentAdmissionConflict) {
		t.Fatalf("conflict: %v", err)
	}
	usage, err := s.UserDocumentUsage(ctx, DocumentScope{UserID: "user"})
	if err != nil || usage.DocumentCount != 1 || usage.ReservedTextBytes != DocumentMaxTextBytes {
		t.Fatalf("usage %+v %v", usage, err)
	}
	if _, err = s.ReadUserDocument(ctx, "other", d.ID, 0, 1); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("isolation: %v", err)
	}
	if _, err = s.DeleteUserDocuments(ctx, DocumentScope{UserID: "other"}, []string{d.ID}); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("delete isolation: %v", err)
	}
	other := acceptDocument(t, s, "other")
	if _, err = s.DeleteUserDocuments(ctx, DocumentScope{UserID: "user"}, []string{d.ID, other.ID}); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("delete rollback: %v", err)
	}
	if _, err = s.ReadUserDocument(ctx, "user", d.ID, 0, 1); err != nil {
		t.Fatal("partial delete committed", err)
	}
	page, err := s.PageUserDocuments(ctx, DocumentScope{Global: true}, "", 1)
	if err != nil || len(page.Documents) != 1 || !page.HasMore {
		t.Fatalf("page %+v %v", page, err)
	}
	next, err := s.PageUserDocuments(ctx, DocumentScope{Global: true}, page.NextAfter, 1)
	if err != nil || len(next.Documents) != 1 || next.HasMore || page.Documents[0].ID == next.Documents[0].ID {
		t.Fatalf("next %+v %v", next, err)
	}
	deleted, err := s.DeleteUserDocuments(ctx, DocumentScope{Global: true}, []string{d.ID, other.ID})
	if err != nil || deleted.DeletedCount != 2 {
		t.Fatalf("global delete %+v %v", deleted, err)
	}
	for _, table := range []string{"user_documents", "user_document_sources", "user_document_extraction_jobs", "user_document_chunks"} {
		assertStoreCount(t, s.sql, `SELECT count(*) FROM `+table, 0)
	}
}

func TestUserDocumentExtractionLeasePublicationAndSearch(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "other")
	ctx := context.Background()
	d := acceptDocument(t, s, "user")
	job := claimDocument(t, s)
	if !bytes.Equal(job.Data, documentUpload("").Data) || job.DocumentID != d.ID || job.Attempts != 1 {
		t.Fatal("wrong source snapshot")
	}
	if j, err := s.ClaimUserDocumentExtraction(ctx, "second"); err != nil || j != nil {
		t.Fatalf("double claim: %v %v", j, err)
	}
	renewed, err := s.RenewUserDocumentExtraction(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	chunks := []DocumentChunk{{Ordinal: 0, Text: "Alpha literal %_ needle", Locator: "page 1", Method: "text"}, {Ordinal: 1, Text: "Second record"}}
	if err = s.CompleteUserDocumentExtraction(ctx, job, chunks, false); !errors.Is(err, ErrDocumentLease) {
		t.Fatalf("stale complete: %v", err)
	}
	if err = s.FailUserDocumentExtraction(ctx, job, "failure"); !errors.Is(err, ErrDocumentLease) {
		t.Fatalf("stale failure: %v", err)
	}
	if _, err = s.RenewUserDocumentExtraction(ctx, job); !errors.Is(err, ErrDocumentLease) {
		t.Fatalf("stale renewal: %v", err)
	}
	if err = s.CompleteUserDocumentExtraction(ctx, renewed, []DocumentChunk{{Ordinal: 1, Text: "wrong ordinal"}}, false); !errors.Is(err, ErrDocumentInvalid) {
		t.Fatal(err)
	}
	if _, err = s.sql.Exec(`CREATE TRIGGER document_fail_insert BEFORE INSERT ON user_document_chunks WHEN NEW.ordinal=1 BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteUserDocumentExtraction(ctx, renewed, chunks, false); err == nil {
		t.Fatal("expected publication rollback")
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_chunks`, 0)
	if _, err = s.sql.Exec(`DROP TRIGGER document_fail_insert`); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteUserDocumentExtraction(ctx, renewed, chunks, true); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteUserDocumentExtraction(ctx, renewed, chunks, true); !errors.Is(err, ErrDocumentLease) {
		t.Fatal("terminal lease reused", err)
	}
	read, err := s.ReadUserDocument(ctx, "user", d.ID, 0, 1)
	if err != nil || read.Document.Status != "partial" || len(read.Chunks) != 1 || !read.HasMore || read.NextOffset != 1 {
		t.Fatalf("read %+v %v", read, err)
	}
	read, err = s.ReadUserDocument(ctx, "user", d.ID, read.NextOffset, 1)
	if err != nil || read.HasMore || read.Chunks[0].Text != chunks[1].Text {
		t.Fatalf("continuation %+v %v", read, err)
	}
	results, err := s.SearchUserDocuments(ctx, "user", "%_", d.ID, 5)
	if err != nil || len(results) != 1 || results[0].Document.ID != d.ID {
		t.Fatalf("literal search %+v %v", results, err)
	}
	results, err = s.SearchUserDocuments(ctx, "other", "alpha", "", 5)
	if err != nil || len(results) != 0 {
		t.Fatalf("search leaked %+v %v", results, err)
	}
	usage, err := s.UserDocumentUsage(ctx, DocumentScope{UserID: "user"})
	if err != nil || usage.ReservedTextBytes != 0 || usage.TextBytes != int64(len(chunks[0].Text)+len(chunks[0].Locator)+len(chunks[0].Method)+len(chunks[1].Text)) {
		t.Fatalf("usage %+v %v", usage, err)
	}
}

func TestUserDocumentsExpiryMaintenanceAndRollback(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	d := acceptDocument(t, s, "user")
	job := claimDocument(t, s)
	now := time.Now()
	if _, err := s.sql.Exec(`UPDATE user_documents SET accepted_at=?,expires_at=? WHERE id=?`, now.Add(-DocumentLifetime-time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli(), d.ID); err != nil {
		t.Fatal(err)
	}
	if docs, err := s.ListUserDocuments(ctx, DocumentScope{UserID: "user"}); err != nil || len(docs) != 0 {
		t.Fatalf("expired list %v %v", docs, err)
	}
	if _, err := s.ReadUserDocument(ctx, "user", d.ID, 0, 1); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatal(err)
	}
	if err := s.CompleteUserDocumentExtraction(ctx, job, nil, false); !errors.Is(err, ErrDocumentLease) {
		t.Fatal(err)
	}
	if j, err := s.ClaimUserDocumentExtraction(ctx, "worker"); err != nil || j != nil {
		t.Fatal("expired claim", err)
	}
	if _, err := s.sql.Exec(`CREATE TRIGGER document_cleanup_fail BEFORE DELETE ON user_documents BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	counts, err := s.MaintenanceSweep(ctx, now, config.DefaultRetentionPolicy())
	if err == nil || counts.UserDocumentsDeleted != 0 {
		t.Fatalf("rollback counts %+v %v", counts, err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 1)
	if _, err = s.sql.Exec(`DROP TRIGGER document_cleanup_fail`); err != nil {
		t.Fatal(err)
	}
	counts, err = s.MaintenanceSweep(ctx, now, config.DefaultRetentionPolicy())
	if err != nil || counts.UserDocumentsDeleted != 1 || counts.Changed() < 1 {
		t.Fatalf("cleanup %+v %v", counts, err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 0)
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_extraction_jobs`, 0)
}

func TestUserDocumentRestartReclaimAndBan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	s := newTestStore(path, config.NewLogger(config.LevelError))
	seedAccountUsers(t, s, "user")
	ctx := context.Background()
	d := acceptDocument(t, s, "user")
	old := claimDocument(t, s)
	if _, err := s.sql.Exec(`UPDATE user_document_extraction_jobs SET lease_until=?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = newTestStore(path, config.NewLogger(config.LevelError))
	defer s.Close()
	fresh := claimDocument(t, s)
	if fresh.DocumentID != d.ID || fresh.Attempts != 2 || fresh.LeaseToken == old.LeaseToken {
		t.Fatal("bad reclaim")
	}
	if err := s.CompleteUserDocumentExtraction(ctx, old, nil, false); !errors.Is(err, ErrDocumentLease) {
		t.Fatal(err)
	}
	if _, err := s.sql.Exec(`UPDATE account_users SET is_banned=1 WHERE canonical_user_id='user'`); err != nil {
		t.Fatal(err)
	}
	if err := s.FailUserDocumentExtraction(ctx, fresh, "unavailable"); !errors.Is(err, ErrDocumentLease) {
		t.Fatal("banned publish", err)
	}
	if _, err := s.sql.Exec(`UPDATE account_users SET is_banned=0 WHERE canonical_user_id='user'`); err != nil {
		t.Fatal(err)
	}
	if err := s.FailUserDocumentExtraction(ctx, fresh, "unsupported_format"); err != nil {
		t.Fatal(err)
	}
	usage, err := s.UserDocumentUsage(ctx, DocumentScope{UserID: "user"})
	if err != nil || usage.ReservedTextBytes != 0 {
		t.Fatalf("failure reserve %+v %v", usage, err)
	}
	read, err := s.ReadUserDocument(ctx, "user", d.ID, 0, 1)
	if err != nil || read.Document.Status != "failed" {
		t.Fatalf("failed status %+v %v", read, err)
	}
}

func TestUserDocumentsReservationsAndConcurrentAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	s := newTestStore(path, config.NewLogger(config.LevelError))
	t.Cleanup(func() { s.Close() })
	seedAccountUsers(t, s, "user")
	other := newTestStore(path, config.NewLogger(config.LevelError))
	t.Cleanup(func() { other.Close() })
	ctx := context.Background()
	for i := 0; i < 24; i++ {
		acceptDocument(t, s, "user")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.AcceptUserDocuments(ctx, "user", []DocumentUpload{documentUpload("")})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	succeeded, rejected := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrDocumentQuota) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("admissions %d %d", succeeded, rejected)
	}
	usage, err := s.UserDocumentUsage(ctx, DocumentScope{UserID: "user"})
	if err != nil || usage.DocumentCount != 25 || usage.ReservedTextBytes != DocumentMaxUserTextBytes {
		t.Fatalf("reservation %+v %v", usage, err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 25)
	job := claimDocument(t, s)
	if err = s.FailUserDocumentExtraction(ctx, job, "empty"); err != nil {
		t.Fatal(err)
	}
	acceptDocument(t, s, "user")
}

func TestUserDocumentAdmissionEnforcesAllStoredQuotas(t *testing.T) {
	// Deliberate accounting fixtures avoid allocating gigabytes of synthetic BLOBs.
	// They exercise transactional admission using the same canonical byte counters.
	for _, test := range []struct {
		name         string
		count        int
		source, text int64
		global       bool
	}{
		{"user_count", 50, 1, 0, false},
		{"user_source", 13, DocumentMaxUserSourceBytes, 0, false},
		{"user_text", 25, 1, DocumentMaxUserTextBytes, false},
		{"global_source", 103, DocumentMaxGlobalSourceBytes, 0, true},
		{"global_text", 200, 1, DocumentMaxGlobalTextBytes, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newFormationTestStore(t)
			ctx := context.Background()
			tx, err := s.sql.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			remainingSource, remainingText := test.source, test.text
			for i := 0; i < test.count; i++ {
				user := "user"
				if test.global {
					user = fmt.Sprintf("quota-%d", i/12)
					if _, err = tx.Exec(`INSERT OR IGNORE INTO account_users(canonical_user_id) VALUES(?)`, user); err != nil {
						t.Fatal(err)
					}
				}
				source := min(int64(DocumentMaxSourceBytes), remainingSource)
				if source < 1 {
					source = 1
				}
				remainingSource -= source
				text := min(int64(DocumentMaxTextBytes), remainingText)
				remainingText -= text
				if _, err = tx.Exec(`INSERT INTO user_documents(id,canonical_user_id,filename,media_type,source_hash,status,source_bytes,text_bytes,reserved_bytes,accepted_at,expires_at) VALUES(?,?, 'quota.txt','text/plain','hash','ready',?,?,0,?,?)`, fmt.Sprintf("quota-%d", i), user, source, text, time.Now().UnixMilli(), time.Now().Add(DocumentLifetime).UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			// A trigger failure would replace ErrDocumentQuota if admission touched
			// any BLOB before checking the complete canonical accounting snapshot.
			if _, err = s.sql.Exec(`CREATE TRIGGER forbid_document_blob BEFORE INSERT ON user_document_sources BEGIN SELECT RAISE(ABORT,'blob inserted before quota check'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ReserveUserDocumentUpload(ctx, "user", 1, 1); !errors.Is(err, ErrDocumentQuota) {
				t.Fatalf("reservation bypassed %s: %v", test.name, err)
			}
			if _, err = s.AcceptUserDocuments(ctx, "user", []DocumentUpload{documentUpload("")}); !errors.Is(err, ErrDocumentQuota) {
				t.Fatalf("admission bypassed %s: %v", test.name, err)
			}
			assertStoreCount(t, s.sql, `SELECT count(*) FROM user_documents`, test.count)
			assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 0)
			assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_extraction_jobs`, 0)
		})
	}
}

func TestUserDocumentQuotaBoundaries(t *testing.T) {
	for _, global := range []bool{false, true} {
		source, text := int64(DocumentMaxUserSourceBytes), int64(DocumentMaxUserTextBytes)
		if global {
			source, text = DocumentMaxGlobalSourceBytes, DocumentMaxGlobalTextBytes
		}
		for _, u := range []DocumentUsage{{DocumentCount: 50, SourceBytes: source, TextBytes: text}, {DocumentCount: 50, SourceBytes: source, ReservedTextBytes: text}} {
			if err := documentQuota(u, global); err != nil {
				t.Fatal(err)
			}
		}
		for _, u := range []DocumentUsage{{SourceBytes: source + 1}, {TextBytes: text, ReservedTextBytes: 1}, {ReservedTextBytes: text + 1}} {
			if !errors.Is(documentQuota(u, global), ErrDocumentQuota) {
				t.Fatal("quota accepted", u)
			}
		}
	}
	if !errors.Is(documentQuota(DocumentUsage{DocumentCount: 51}, false), ErrDocumentQuota) {
		t.Fatal("count quota")
	}
	s := newFormationTestStore(t)
	ctx := context.Background()
	oversized := documentUpload("")
	oversized.Data = make([]byte, DocumentMaxSourceBytes+1)
	if _, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{oversized}); !errors.Is(err, ErrDocumentQuota) {
		t.Fatal(err)
	}
	full := documentUpload("")
	full.Data = make([]byte, DocumentMaxSourceBytes)
	if _, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{full, full, documentUpload("")}); !errors.Is(err, ErrDocumentQuota) {
		t.Fatal(err)
	}
	if _, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{full, full}); err != nil {
		t.Fatal("40MiB boundary", err)
	}
	if _, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{documentUpload(""), documentUpload(""), documentUpload(""), documentUpload(""), documentUpload("")}); !errors.Is(err, ErrDocumentInvalid) {
		t.Fatal(err)
	}
	job := claimDocument(t, s)
	chunks := make([]DocumentChunk, 65)
	for i := range chunks {
		chunks[i] = DocumentChunk{Ordinal: i, Text: strings.Repeat("x", DocumentMaxChunkBytes)}
	}
	if err := s.CompleteUserDocumentExtraction(ctx, job, chunks, false); !errors.Is(err, ErrDocumentQuota) {
		t.Fatal(err)
	}
	if err := s.CompleteUserDocumentExtraction(ctx, job, chunks[:64], false); err != nil {
		t.Fatal("1MiB boundary", err)
	}
}

func TestUserDocumentsMergeResetAndForget(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "winner")
	ctx := context.Background()
	docs, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{documentUpload("surviving-key")})
	if err != nil {
		t.Fatal(err)
	}
	d := docs[0]
	old := claimDocument(t, s)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = MergeUsersTx(ctx, tx, "winner", "user", ""); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.AcceptUserDocuments(ctx, "winner", []DocumentUpload{documentUpload("surviving-key")})
	if err != nil || len(replayed) != 1 || replayed[0].ID != d.ID {
		t.Fatalf("merge replay: %+v %v", replayed, err)
	}
	if err = s.CompleteUserDocumentExtraction(ctx, old, nil, false); !errors.Is(err, ErrDocumentLease) {
		t.Fatal("merge lease", err)
	}
	fresh := claimDocument(t, s)
	if fresh.UserID != "winner" || fresh.DocumentID != d.ID {
		t.Fatal("merge source")
	}
	if err = s.CompleteUserDocumentExtraction(ctx, fresh, []DocumentChunk{{Text: "merged"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResetSession(ctx, "winner", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadUserDocument(ctx, "winner", d.ID, 0, 1); err != nil {
		t.Fatal("reset erased library", err)
	}
	if _, err = s.ResetUserDataPreservingAccount(ctx, "winner", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"user_documents", "user_document_sources", "user_document_chunks", "user_document_extraction_jobs"} {
		assertStoreCount(t, s.sql, `SELECT count(*) FROM `+table, 0)
	}
	acceptDocument(t, s, "winner")
	tx, err = s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteUserTx(ctx, tx, "winner", time.Now()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 0)
}

func TestUserDocumentMergeQuotaRollsBack(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "winner")
	ctx := context.Background()
	for i := 0; i < 13; i++ {
		acceptDocument(t, s, "user")
		acceptDocument(t, s, "winner")
	}
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = MergeUsersTx(ctx, tx, "winner", "user", "")
	if !errors.Is(err, ErrDocumentQuota) {
		tx.Rollback()
		t.Fatalf("merge quota: %v", err)
	}
	tx.Rollback()
	for _, user := range []string{"user", "winner"} {
		usage, err := s.UserDocumentUsage(ctx, DocumentScope{UserID: user})
		if err != nil || usage.DocumentCount != 13 {
			t.Fatalf("rollback %+v %v", usage, err)
		}
	}
}

func TestUserDocumentLogsDoNotExposePayloads(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			s := newFormationTestStore(t)
			var output bytes.Buffer
			s.log = config.NewLogger(level)
			s.log.SetOutput(&output)
			u := documentUpload("private_admission_canary")
			u.Filename = "private_filename_canary"
			u.Data = []byte("private_source_canary")
			if _, err := s.AcceptUserDocuments(context.Background(), "user", []DocumentUpload{u}); err != nil {
				t.Fatal(err)
			}
			logs := output.String()
			if strings.Contains(logs, "canary") || strings.Count(logs, `"event":"memory.documents.complete"`) != 1 || !strings.Contains(logs, `"level":"info"`) {
				t.Fatalf("unsafe or missing log %s", logs)
			}
		})
	}
}
