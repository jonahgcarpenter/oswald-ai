package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

func buildDocumentTestIndex(t *testing.T, s *Store, kind string) DerivedIndexRevision {
	t.Helper()
	model, dimension := "", 0
	if kind == IndexKindUserDocumentVector {
		model, dimension = "live-model", 2
	}
	r, err := s.CreateIndexRevision(context.Background(), kind, providerForKind(kind), model, dimension)
	if err != nil {
		t.Fatal(err)
	}
	records, err := s.UserDocumentIndexRecords(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := s.WriteUserDocumentIndexRecord(context.Background(), r, record, []float64{1, 0}); err != nil {
			t.Fatal(err)
		}
	}
	r, err = s.ValidateAndPublishIndexRevision(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDocumentIndexFallbackMonitoringAndReceiptRetention(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	d := publishSearchDocument(t, s, "user", false, "canarydocumentword private content")
	r := buildDocumentTestIndex(t, s, IndexKindUserDocumentFTS)
	change, err := s.ClaimDerivedIndexChange(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteDerivedIndexChange(ctx, change); err != nil {
		t.Fatal(err)
	}
	if _, err = s.sql.Exec(`UPDATE durable_jobs SET completed_at=? WHERE id=?`, formatTime(time.Now().Add(-8*24*time.Hour)), change.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MaintenanceSweep(ctx, time.Now(), config.DefaultRetentionPolicy()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.sql.QueryRow(`SELECT count(*) FROM durable_jobs WHERE id=?`, change.Sequence).Scan(&count); err != nil || count != 1 {
		t.Fatal("live receipt removed", count, err)
	}
	if _, err = s.sql.Exec(`DROP TABLE ` + r.TableName); err != nil {
		t.Fatal(err)
	}
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		var logs bytes.Buffer
		s.log = config.NewLogger(level)
		s.log.SetOutput(&logs)
		got, err := s.SearchUserDocuments(ctx, "user", "canarydocumentword", d.ID, 5)
		if err != nil || len(got) != 1 {
			t.Fatal(got, err)
		}
		if strings.Contains(logs.String(), "canarydocumentword") || strings.Contains(logs.String(), d.ID) {
			t.Fatal("private data in logs")
		}
		terminal, warnings := 0, 0
		for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatal(err)
			}
			if event["event"] == "user_document.search.index_degraded" {
				warnings++
				if event["level"] != "warn" {
					t.Fatal(event)
				}
			}
			if event["operation"] == "document_search" {
				terminal++
			}
		}
		if warnings != 1 || terminal != 1 {
			t.Fatalf("measurement counts warnings=%d terminal=%d: %s", warnings, terminal, logs.String())
		}
	}
}

type documentQueryEmbedder struct {
	calls   int
	model   string
	onEmbed func() error
}

func (e *documentQueryEmbedder) Embed(_ context.Context, r llm.EmbedRequest) (*llm.EmbedResponse, error) {
	e.calls++
	e.model = r.Model
	if e.onEmbed != nil {
		if err := e.onEmbed(); err != nil {
			return nil, err
		}
	}
	return &llm.EmbedResponse{Embeddings: [][]float64{{1, 0}}}, nil
}

func TestDocumentSearchSkipsCandidateDeletedDuringEmbedding(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	removed := publishSearchDocument(t, s, "user", false, "Renewal deadline for the removed document.")
	survivor := publishSearchDocument(t, s, "user", false, "Renewal deadline for the surviving document.")
	buildDocumentTestIndex(t, s, IndexKindUserDocumentFTS)
	buildDocumentTestIndex(t, s, IndexKindUserDocumentVector)
	e := &documentQueryEmbedder{onEmbed: func() error {
		_, err := s.DeleteUserDocuments(ctx, DocumentScope{UserID: "user"}, []string{removed.ID})
		return err
	}}
	s.embedder, s.embedModel = e, "live-model"
	results, err := s.SearchUserDocuments(ctx, "user", "renewal deadline", "", 10)
	if err != nil || len(results) != 1 || results[0].Document.ID != survivor.ID {
		t.Fatalf("surviving search results: %+v, %v", results, err)
	}
	if e.calls != 1 {
		t.Fatalf("embedding calls = %d, want 1", e.calls)
	}
	if _, err := s.ReadUserDocument(ctx, "user", removed.ID, 0, 1); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("candidate was not deleted during embedding: %v", err)
	}
}

func TestDocumentIndexWakeupsFollowCommittedPublicationAndDeletion(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	d := acceptDocument(t, s, "user")
	job := claimDocument(t, s)
	wakeups, wantRecords := 0, 1
	s.SetDerivedIndexNotifier(func() {
		wakeups++
		// A separate query must observe committed state, not an open write transaction.
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		records, err := s.UserDocumentIndexRecords(readCtx, 0, 10)
		if err != nil || len(records) != wantRecords {
			t.Errorf("notified before committed document state: %d records, %v", len(records), err)
		}
	})
	chunks := []DocumentChunk{{Ordinal: 0, Text: "Synthetic indexed document.", Method: "text"}}
	if err := s.CompleteUserDocumentExtraction(ctx, job, chunks, false); err != nil {
		t.Fatal(err)
	}
	if wakeups != 1 {
		t.Fatalf("publication wakeups = %d, want 1", wakeups)
	}
	if err := s.CompleteUserDocumentExtraction(ctx, job, chunks, false); err == nil {
		t.Fatal("stale publication succeeded")
	}
	if _, err := s.DeleteUserDocuments(ctx, DocumentScope{UserID: "user"}, []string{d.ID, "missing"}); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("expected deletion rollback: %v", err)
	}
	if wakeups != 1 {
		t.Fatalf("failed operations signaled indexing: %d wakeups", wakeups)
	}
	wantRecords = 0
	if _, err := s.DeleteUserDocuments(ctx, DocumentScope{UserID: "user"}, []string{d.ID}); err != nil {
		t.Fatal(err)
	}
	if wakeups != 2 {
		t.Fatalf("deletion wakeups = %d, want 2", wakeups)
	}
}

func TestDocumentIndexesSearchLagIsolationExpiryAndLiveModel(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "other")
	ctx := context.Background()
	d := publishSearchDocument(t, s, "user", true, "The automobile needs servicing. Caf\u00e9.")
	publishSearchDocument(t, s, "other", false, "Private automobile information.")
	fts := buildDocumentTestIndex(t, s, IndexKindUserDocumentFTS)
	vector := buildDocumentTestIndex(t, s, IndexKindUserDocumentVector)
	e := &documentQueryEmbedder{}
	s.embedder = e
	// Disabled configuration never calls an embedder, even with a retained index.
	got, err := s.SearchUserDocuments(ctx, "user", "car", "", 10)
	if err != nil || len(got) != 0 || e.calls != 0 {
		t.Fatalf("disabled search: %v %v calls=%d", got, err, e.calls)
	}
	got, err = s.SearchUserDocuments(ctx, "user", "cafe", "", 10)
	if err != nil || len(got) != 1 || got[0].Document.ID != d.ID || e.calls != 0 {
		t.Fatalf("FTS diacritic match: %v %v calls=%d", got, err, e.calls)
	}
	s.embedModel = "replacement-model"
	got, err = s.SearchUserDocuments(ctx, "user", "car", "", 10)
	if err != nil || len(got) != 1 || got[0].Document.ID != d.ID || e.model != "live-model" {
		t.Fatalf("semantic: %v %v model=%s", got, err, e.model)
	}
	late := publishSearchDocument(t, s, "user", false, "Unique laggedword current publication.")
	got, err = s.SearchUserDocuments(ctx, "user", "laggedword", late.ID, 10)
	if err != nil || len(got) != 1 || got[0].Document.ID != late.ID {
		t.Fatalf("lag: %v %v", got, err)
	}
	if _, err = s.sql.Exec(`UPDATE user_documents SET expires_at=accepted_at+1 WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.SearchUserDocuments(ctx, "user", "car", d.ID, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("expiry: %v %v", got, err)
	}
	if _, err = s.MaintainDerivedIndexes(ctx, time.Now(), time.Hour, 100); err != nil {
		t.Fatal(err)
	}
	for _, r := range []DerivedIndexRevision{fts, vector} {
		var count int
		if err := s.sql.QueryRow(`SELECT count(*) FROM ` + r.TableName + ` WHERE canonical_user_id='user'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cleanup %s: %d %v", r.Kind, count, err)
		}
	}
	if _, err = s.DeleteUserDocuments(ctx, DocumentScope{UserID: "user"}, []string{late.ID}); err != nil {
		t.Fatal(err)
	}
	got, err = s.SearchUserDocuments(ctx, "user", "laggedword", late.ID, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("deleted: %v %v", got, err)
	}
}

func TestDocumentIndexOutboxFencingAndNonReusableIDs(t *testing.T) {
	s := newFormationTestStore(t)
	ctx := context.Background()
	d := publishSearchDocument(t, s, "user", false, "Synthetic chunk.")
	records, err := s.UserDocumentIndexRecords(ctx, 0, 10)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	old := records[0]
	r := buildDocumentTestIndex(t, s, IndexKindUserDocumentFTS)
	change, err := s.ClaimDerivedIndexChange(ctx, time.Minute)
	if err != nil || change.EntityKind != "user_document_chunk" || change.EntityID != old.ID {
		t.Fatal(change, err)
	}
	if err = s.CompleteDerivedIndexChange(ctx, change); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteUserDocuments(ctx, DocumentScope{UserID: "user"}, []string{d.ID}); err != nil {
		t.Fatal(err)
	}
	if err = s.WriteUserDocumentIndexRecord(ctx, r, old, nil); !errors.Is(err, ErrStaleIndexRecord) {
		t.Fatal("stale publication", err)
	}
	change, err = s.ClaimDerivedIndexChange(ctx, time.Minute)
	if err != nil || change.Operation != "delete" || change.EntityID != old.ID {
		t.Fatal(change, err)
	}
	publishSearchDocument(t, s, "user", false, "New chunk.")
	records, err = s.UserDocumentIndexRecords(ctx, 0, 10)
	if err != nil || len(records) != 1 || records[0].ID <= old.ID {
		t.Fatal(records, err)
	}
	if _, err = s.WritableIndexRevisions(ctx, "unknown"); err == nil {
		t.Fatal("unknown entity accepted")
	}
}

func TestDocumentIndexStaleOwnerCannotDeleteMergedProjection(t *testing.T) {
	s := newFormationTestStore(t)
	seedAccountUsers(t, s, "winner")
	ctx := context.Background()
	publishSearchDocument(t, s, "user", false, "Synthetic private chunk.")
	records, err := s.UserDocumentIndexRecords(ctx, 0, 10)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	old := records[0]
	for _, kind := range []string{IndexKindUserDocumentFTS, IndexKindUserDocumentVector} {
		t.Run(kind, func(t *testing.T) {
			if _, err := s.sql.Exec(`UPDATE user_documents SET canonical_user_id='user'`); err != nil {
				t.Fatal(err)
			}
			r := buildDocumentTestIndex(t, s, kind)
			if _, err := s.sql.Exec(`UPDATE user_documents SET canonical_user_id='winner'`); err != nil {
				t.Fatal(err)
			}
			current, err := s.UserDocumentIndexRecordByID(ctx, old.ID, "winner")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.WriteUserDocumentIndexRecord(ctx, r, current, []float64{1, 0}); err != nil {
				t.Fatal(err)
			}
			if err := s.WriteUserDocumentIndexRecord(ctx, r, old, []float64{1, 0}); !errors.Is(err, ErrStaleIndexRecord) {
				t.Fatal(err)
			}
			var count int
			if err := s.sql.QueryRow(`SELECT count(*) FROM ` + r.TableName + ` WHERE canonical_user_id='winner'`).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}
