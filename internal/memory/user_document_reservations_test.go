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

func TestDocumentUploadReservationValidationAndReleaseFailure(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	for _, input := range []struct {
		count int
		bytes int64
	}{{0, 1}, {5, 1}, {1, 0}, {2, 1}, {1, DocumentMaxSourceBytes + 1}, {4, DocumentMaxRequestBytes + 1}} {
		if _, err := s.ReserveUserDocumentUpload(ctx, "user", input.count, input.bytes); err == nil {
			t.Fatalf("invalid reservation %+v", input)
		}
	}
	if _, err := s.ReserveUserDocumentUpload(ctx, "missing", 1, 1); err == nil {
		t.Fatal("missing owner reserved")
	}
	r := reserveDocumentUpload(t, s, "user", 2, DocumentMaxRequestBytes)
	if err := s.ReleaseUserDocumentUpload(ctx, "user", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseUserDocumentUpload(ctx, "user", r.ID); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserDocumentUsage(ctx, DocumentScope{Global: true})
	if err != nil || u.ReservedSourceBytes != 0 || u.ReservedTextBytes != 0 || u.ReservedDocumentCount != 0 {
		t.Fatalf("release usage %+v %v", u, err)
	}
	r = reserveDocumentUpload(t, s, "user", 1, 100)
	if _, err = s.sql.Exec(`CREATE TRIGGER prevent_reservation_release BEFORE DELETE ON user_document_reservations BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptReservedUserDocuments(ctx, "user", r.ID, nil); !errors.Is(err, ErrDocumentInvalid) {
		t.Fatalf("release failure lost original error: %v", err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 1)
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_documents`, 0)
	expireDocumentReservation(t, s, r.ID)
	u, err = s.UserDocumentUsage(ctx, DocumentScope{Global: true})
	if err != nil || u.ReservedTextBytes != 0 || u.ReservedSourceBytes != 0 {
		t.Fatalf("unreleased expiry usage %+v %v", u, err)
	}
}

func TestDocumentUploadReservationCleanupIsBounded(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	expires := time.Now().Add(-time.Second)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 105; i++ {
		if _, err = tx.Exec(`INSERT INTO user_document_reservations(id,canonical_user_id,file_count,source_bytes,reserved_text_bytes,created_at,expires_at) VALUES(?,'user',1,1,1048576,?,?)`, fmt.Sprintf("expired-%d", i), expires.Add(-DocumentReservationLifetime).UnixMilli(), expires.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	reserveDocumentUpload(t, s, "user", 1, 100)
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 6)
	policy := config.DefaultRetentionPolicy()
	policy.BatchSize = 2
	counts, err := s.MaintenanceSweep(ctx, time.Now(), policy)
	if err != nil || counts.UserDocumentReservationsDeleted != 2 {
		t.Fatalf("bounded sweep %+v %v", counts, err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 4)
}

func TestDocumentUploadReservationConcurrentConsumption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consume.db")
	s := newTestStore(path, config.NewLogger(config.LevelError))
	defer s.Close()
	seedAccountUsers(t, s, "user")
	other := newTestStore(path, config.NewLogger(config.LevelError))
	defer other.Close()
	r := reserveDocumentUpload(t, s, "user", 1, 100)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.AcceptReservedUserDocuments(context.Background(), "user", r.ID, []DocumentUpload{documentUpload("")})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	accepted, rejected := 0, 0
	for err := range errs {
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrDocumentReservation) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("concurrent consume %d %d", accepted, rejected)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_documents`, 1)
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 1)
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 0)
}

func TestDocumentUploadReservationLogCountsAreCommittedAndPrivate(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			s := newFormationTestStore(t)
			var output bytes.Buffer
			s.log = config.NewLogger(level)
			s.log.SetOutput(&output)
			r := reserveDocumentUpload(t, s, "user", 1, 100)
			u := documentUpload("private_admission_canary")
			u.Filename = "private_filename_canary"
			u.Data = []byte("private_source_canary")
			if _, err := s.AcceptReservedUserDocuments(context.Background(), "user", r.ID, []DocumentUpload{u}); err != nil {
				t.Fatal(err)
			}
			logs := output.String()
			if strings.Contains(logs, "canary") || strings.Contains(logs, r.ID) || strings.Count(logs, `"event":"memory.documents.complete"`) != 2 || !strings.Contains(logs, `"reserved_count":1`) || !strings.Contains(logs, `"accepted_count":1`) {
				t.Fatalf("unsafe or missing measurements %s", logs)
			}
			output.Reset()
			r = reserveDocumentUpload(t, s, "user", 1, 100)
			output.Reset()
			if _, err := s.AcceptReservedUserDocuments(context.Background(), "user", r.ID, nil); !errors.Is(err, ErrDocumentInvalid) {
				t.Fatal(err)
			}
			logs = output.String()
			if strings.Count(logs, `"event":"memory.documents.complete"`) != 2 || !strings.Contains(logs, `"accepted_count":0`) || !strings.Contains(logs, `"released_count":1`) {
				t.Fatalf("rollback/release measurements %s", logs)
			}
		})
	}
}

func reserveDocumentUpload(t *testing.T, s *Store, user string, count int, source int64) DocumentReservation {
	t.Helper()
	r, err := s.ReserveUserDocumentUpload(context.Background(), user, count, source)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func expireDocumentReservation(t *testing.T, s *Store, id string) {
	t.Helper()
	expires := time.Now().Add(-time.Second)
	if _, err := s.sql.Exec(`UPDATE user_document_reservations SET created_at=?,expires_at=? WHERE id=?`, expires.Add(-DocumentReservationLifetime).UnixMilli(), expires.UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentUploadReservationAccountingConsumptionAndReplay(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	r := reserveDocumentUpload(t, s, "user", 4, 100)
	if r.UserID != "user" || len(r.ID) != 32 || r.FileCount != 4 || r.SourceBytes != 100 || r.ReservedTextBytes != 4*DocumentMaxTextBytes || r.ExpiresAt.Sub(r.CreatedAt) != DocumentReservationLifetime {
		t.Fatalf("reservation %+v", r)
	}
	u, err := s.UserDocumentUsage(ctx, DocumentScope{UserID: "user"})
	if err != nil || u.DocumentCount != 0 || u.SourceBytes != 0 || u.ReservationCount != 1 || u.ReservedDocumentCount != 4 || u.ReservedSourceBytes != 100 || u.UploadReservedTextBytes != 4*DocumentMaxTextBytes || u.ReservedTextBytes != u.UploadReservedTextBytes {
		t.Fatalf("usage %+v %v", u, err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 0)
	upload := documentUpload("replay")
	docs, err := s.AcceptReservedUserDocuments(ctx, "user", r.ID, []DocumentUpload{upload})
	if err != nil || len(docs) != 1 {
		t.Fatalf("accept %v %v", docs, err)
	}
	u, err = s.UserDocumentUsage(ctx, DocumentScope{UserID: "user"})
	if err != nil || u.DocumentCount != 1 || u.QueuedCount != 1 || u.SourceBytes != int64(len(upload.Data)) || u.ReservationCount != 0 || u.ReservedSourceBytes != 0 || u.UploadReservedTextBytes != 0 || u.ReservedDocumentCount != 0 || u.ReservedTextBytes != DocumentMaxTextBytes {
		t.Fatalf("consumed usage %+v %v", u, err)
	}
	if _, err = s.AcceptReservedUserDocuments(ctx, "user", r.ID, []DocumentUpload{upload}); !errors.Is(err, ErrDocumentReservation) {
		t.Fatalf("consumed replay %v", err)
	}
	r = reserveDocumentUpload(t, s, "user", 1, 100)
	again, err := s.AcceptReservedUserDocuments(ctx, "user", r.ID, []DocumentUpload{upload})
	if err != nil || len(again) != 1 || again[0].ID != docs[0].ID {
		t.Fatalf("admission-key replay %v %v", again, err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_sources`, 1)
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 0)
	if err = s.ReleaseUserDocumentUpload(ctx, "user", r.ID); err != nil {
		t.Fatal("idempotent release", err)
	}
}

func TestDocumentUploadReservationFailuresReleaseCapacity(t *testing.T) {
	for _, test := range []string{"bytes", "files", "invalid", "canceled", "blob_failure", "conflict", "expired"} {
		t.Run(test, func(t *testing.T) {
			s := newFormationTestStore(t)
			ctx := context.Background()
			r := reserveDocumentUpload(t, s, "user", 1, 100)
			uploads := []DocumentUpload{documentUpload("")}
			want := ErrDocumentQuota
			switch test {
			case "bytes":
				uploads[0].Data = make([]byte, 101)
			case "files":
				uploads = append(uploads, documentUpload(""))
			case "invalid":
				uploads[0].Filename = "../bad"
				want = ErrDocumentInvalid
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
				want = context.Canceled
			case "expired":
				expireDocumentReservation(t, s, r.ID)
				want = ErrDocumentReservation
			case "conflict":
				uploads[0].AdmissionKey = "same"
				if _, err := s.AcceptUserDocuments(ctx, "user", uploads); err != nil {
					t.Fatal(err)
				}
				uploads[0].Data = []byte("different")
				want = ErrDocumentAdmissionConflict
			case "blob_failure":
				if _, err := s.sql.Exec(`CREATE TRIGGER fail_reserved_blob BEFORE INSERT ON user_document_sources BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
					t.Fatal(err)
				}
				want = nil
			}
			_, err := s.AcceptReservedUserDocuments(ctx, "user", r.ID, uploads)
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatalf("failure %v want %v", err, want)
			}
			assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 0)
			if test != "conflict" {
				assertStoreCount(t, s.sql, `SELECT count(*) FROM user_documents`, 0)
			}
		})
	}
}

func TestDocumentUploadReservationIsolationExpiryAndMaintenance(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "other")
	ctx := context.Background()
	r := reserveDocumentUpload(t, s, "user", 1, 100)
	if err := s.ReleaseUserDocumentUpload(ctx, "other", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptReservedUserDocuments(ctx, "other", r.ID, []DocumentUpload{documentUpload("")}); !errors.Is(err, ErrDocumentReservation) {
		t.Fatal("foreign accept", err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 1)
	expireDocumentReservation(t, s, r.ID)
	u, err := s.UserDocumentUsage(ctx, DocumentScope{Global: true})
	if err != nil || u.ReservationCount != 0 || u.ReservedTextBytes != 0 || u.ReservedSourceBytes != 0 {
		t.Fatalf("expired usage %+v %v", u, err)
	}
	if _, err = s.sql.Exec(`CREATE TRIGGER fail_reservation_cleanup BEFORE DELETE ON user_document_reservations BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	counts, err := s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err == nil || counts.UserDocumentReservationsDeleted != 0 {
		t.Fatalf("rollback counts %+v %v", counts, err)
	}
	if _, err = s.sql.Exec(`DROP TRIGGER fail_reservation_cleanup`); err != nil {
		t.Fatal(err)
	}
	counts, err = s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy())
	if err != nil || counts.UserDocumentReservationsDeleted != 1 || counts.Changed() < 1 {
		t.Fatalf("cleanup counts %+v %v", counts, err)
	}
	r = reserveDocumentUpload(t, s, "user", 1, 100)
	expireDocumentReservation(t, s, r.ID)
	reserveDocumentUpload(t, s, "other", 1, 100)
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 1)
}

func TestDocumentUploadReservationConcurrencyAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reservations.db")
	s := newTestStore(path, config.NewLogger(config.LevelError))
	defer s.Close()
	seedAccountUsers(t, s, "user")
	other := newTestStore(path, config.NewLogger(config.LevelError))
	defer other.Close()
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		reserveDocumentUpload(t, s, "user", 4, 100)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	reservations := make(chan DocumentReservation, 2)
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := store.ReserveUserDocumentUpload(ctx, "user", 1, 100)
			reservations <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	close(reservations)
	accepted, rejected := 0, 0
	for err := range errs {
		if err == nil {
			accepted++
		} else if errors.Is(err, ErrDocumentQuota) {
			rejected++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("concurrent results %d %d", accepted, rejected)
	}
	var r DocumentReservation
	for result := range reservations {
		if result.ID != "" {
			r = result
		}
	}
	if _, err := s.AcceptUserDocuments(ctx, "user", []DocumentUpload{documentUpload("")}); !errors.Is(err, ErrDocumentQuota) {
		t.Fatal("direct admission stole reservation", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newTestStore(path, config.NewLogger(config.LevelError))
	defer reopened.Close()
	if _, err := reopened.AcceptReservedUserDocuments(ctx, "user", r.ID, []DocumentUpload{documentUpload("")}); err != nil {
		t.Fatal("reserved acceptance double-counted capacity", err)
	}
	u, err := s.UserDocumentUsage(ctx, DocumentScope{UserID: "user"})
	if err != nil || u.DocumentCount != 1 || u.ReservedDocumentCount != 24 || u.ReservedTextBytes != DocumentMaxUserTextBytes {
		t.Fatalf("usage %+v %v", u, err)
	}
}

func TestDocumentUploadReservationMergeResetAndDeletion(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "winner")
	ctx := context.Background()
	loser := reserveDocumentUpload(t, s, "user", 1, 100)
	winner := reserveDocumentUpload(t, s, "winner", 1, 100)
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = MergeUsersTx(ctx, tx, "winner", "user", ""); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Rollback()
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 2)
	tx, err = s.sql.BeginTx(ctx, nil)
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
	for _, user := range []string{"user", "winner"} {
		if _, err = s.AcceptReservedUserDocuments(ctx, user, loser.ID, []DocumentUpload{documentUpload("")}); err == nil || (user == "winner" && !errors.Is(err, ErrDocumentReservation)) {
			t.Fatal("merged reservation survived", err)
		}
	}
	if _, err = s.ResetSession(ctx, "winner", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptReservedUserDocuments(ctx, "winner", winner.ID, []DocumentUpload{documentUpload("")}); err != nil {
		t.Fatal("winner/reset lost reservation", err)
	}
	reserveDocumentUpload(t, s, "winner", 1, 100)
	if _, err = s.ResetUserDataPreservingAccount(ctx, "winner", time.Now()); err != nil {
		t.Fatal(err)
	}
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 0)
	reserveDocumentUpload(t, s, "winner", 1, 100)
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
	assertStoreCount(t, s.sql, `SELECT count(*) FROM user_document_reservations`, 0)
}

func TestDocumentUsageStatusAndExpiredMetadata(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "other")
	ctx := context.Background()
	for i, status := range []string{"queued", "extracting", "ready", "partial", "failed", "failed"} {
		d := acceptDocument(t, s, "user")
		if _, err := s.sql.Exec(`UPDATE user_documents SET status=? WHERE id=?`, status, d.ID); err != nil {
			t.Fatal(err)
		}
		if i == 5 {
			if _, err := s.sql.Exec(`UPDATE user_documents SET accepted_at=?,expires_at=? WHERE id=?`, time.Now().Add(-31*24*time.Hour).UnixMilli(), time.Now().Add(-time.Second).UnixMilli(), d.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	u, err := s.UserDocumentUsage(ctx, DocumentScope{Global: true})
	if err != nil || u.DocumentCount != 6 || u.ExpiredCount != 1 || u.QueuedCount != 1 || u.RunningCount != 1 || u.ReadyCount != 1 || u.PartialCount != 1 || u.FailedCount != 1 {
		t.Fatalf("status %+v %v", u, err)
	}
	for _, scope := range []DocumentScope{{UserID: "user", IncludeExpired: true}, {Global: true, IncludeExpired: true}} {
		page, err := s.PageUserDocuments(ctx, scope, "", 50)
		if err != nil || len(page.Documents) != 6 {
			t.Fatalf("expired page %+v %v", page, err)
		}
		for _, d := range page.Documents {
			if d.ExpiresAt.Before(time.Now()) {
				if _, err = s.ReadUserDocument(ctx, "user", d.ID, 0, 1); !errors.Is(err, ErrDocumentNotFound) {
					t.Fatal("expired read became eligible", err)
				}
			}
		}
	}
	page, err := s.PageUserDocuments(ctx, DocumentScope{UserID: "user"}, "", 50)
	if err != nil || len(page.Documents) != 5 {
		t.Fatalf("default page %+v %v", page, err)
	}
	page, err = s.PageUserDocuments(ctx, DocumentScope{UserID: "other", IncludeExpired: true}, "", 50)
	if err != nil || len(page.Documents) != 0 {
		t.Fatal("expired scope leaked", err)
	}
	page, err = s.PageUserDocuments(ctx, DocumentScope{UserID: "user", IncludeExpired: true}, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range page.Documents {
		if d.ExpiresAt.Before(time.Now()) {
			if _, err = s.DeleteUserDocuments(ctx, DocumentScope{UserID: "user"}, []string{d.ID}); err != nil {
				t.Fatal("expired deletion", err)
			}
		}
	}
}
