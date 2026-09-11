package documents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

func documentServiceFixture(t *testing.T) (*memory.Store, *database.DB, memory.UserDocument) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "documents.db")
	log := config.NewLogger(config.LevelError)
	db, err := database.Open(path, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('user')`); err != nil {
		t.Fatal(err)
	}
	store := memorytest.NewStore(t, path, log)
	docs, err := store.AcceptUserDocuments(context.Background(), "user", []memory.DocumentUpload{{Filename: "private-canary.txt", MediaType: "text/plain", Data: []byte("private-canary")}})
	if err != nil {
		t.Fatal(err)
	}
	return store, db, docs[0]
}

func TestStorageChunksBounds(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		count      int
		partial    bool
	}{
		{"split", strings.Repeat("a", 40000), 1, false},
		{"utf8", strings.Repeat("\u20ac", 10000), 1, false},
		{"metadata_budget", strings.Repeat("a", memory.DocumentMaxTextBytes), 1, true},
		{"chunk_limit", "a", 1025, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Result{}
			for i := 0; i < tc.count; i++ {
				r.Chunks = append(r.Chunks, Chunk{Ordinal: i + 1, Text: tc.text, Locator: "page:1", Method: "native"})
			}
			chunks, partial := storageChunks(r)
			if partial != tc.partial || len(chunks) == 0 || len(chunks) > 1024 {
				t.Fatalf("count=%d partial=%v", len(chunks), partial)
			}
			var size int
			var text strings.Builder
			for i, c := range chunks {
				if c.Ordinal != i || !utf8.ValidString(c.Text) || len(c.Text) > 16<<10 || c.Locator != "page:1" || c.Method != "native" {
					t.Fatal("invalid storage chunk")
				}
				size += len(c.Text) + len(c.Locator) + len(c.Method)
				text.WriteString(c.Text)
			}
			if size > 1<<20 {
				t.Fatalf("size=%d", size)
			}
			if !partial && text.String() != tc.text {
				t.Fatal("split changed text")
			}
		})
	}
}

func TestDocumentServiceOutcomesAndSafeTelemetry(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		for _, tc := range []struct {
			name, status, code string
			result             Result
			err                error
		}{
			{"ready", "ready", "", Result{Chunks: []Chunk{{Ordinal: 1, Text: "private-canary", Method: "native"}}}, nil},
			{"partial", "partial", "", Result{Partial: true, Chunks: []Chunk{{Ordinal: 1, Text: "private-canary"}}}, nil},
			{"limit", "partial", "", Result{Chunks: []Chunk{{Ordinal: 1, Text: "private-canary"}}}, ErrLimit},
			{"failure", "failed", "extraction_failed", Result{Chunks: []Chunk{{Ordinal: 1, Text: "private-canary"}}}, errors.New("private-canary")},
			{"empty", "failed", "no_extractable_text", Result{}, nil},
			{"unsupported", "failed", "unsupported_format", Result{}, ErrUnsupported},
			{"timeout", "failed", "extraction_timeout", Result{}, context.DeadlineExceeded},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store, db, doc := documentServiceFixture(t)
				job, err := store.ClaimUserDocumentExtraction(context.Background(), "test")
				if err != nil || job == nil {
					t.Fatalf("claim: %v", err)
				}
				var logs bytes.Buffer
				log := config.NewLogger(level)
				log.SetOutput(&logs)
				s := NewService(store, log)
				s.extract = func(ctx context.Context, _, _ string, _ []byte) (Result, error) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 3*time.Minute {
						t.Fatal("missing extraction deadline")
					}
					return tc.result, tc.err
				}
				s.process(context.Background(), job)
				var status, code string
				if err := db.SQL().QueryRow(`SELECT d.status,j.failure_code FROM user_documents d JOIN user_document_extraction_jobs j ON j.document_id=d.id WHERE d.id=?`, doc.ID).Scan(&status, &code); err != nil {
					t.Fatal(err)
				}
				if status != tc.status || code != tc.code {
					t.Fatalf("status=%s code=%s", status, code)
				}
				var record map[string]any
				if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] != "documents.job.complete" || record["level"] != "info" || record["workload"] != "document_extraction" || record["operation_id"] == "" || record["user_id"] != "user" || strings.Contains(logs.String(), "private-canary") {
					t.Fatalf("unsafe/missing telemetry: %s", logs.String())
				}
				if err := store.CompleteUserDocumentExtraction(context.Background(), job, nil, false); !errors.Is(err, memory.ErrDocumentLease) {
					t.Fatalf("old lease usable: %v", err)
				}
			})
		}
	}
}

func TestDocumentServiceStopJoinsAndLeavesReclaimable(t *testing.T) {
	store, db, doc := documentServiceFixture(t)
	started, exited := make(chan struct{}), make(chan struct{})
	s := NewService(store, nil)
	s.extract = func(ctx context.Context, _, _ string, _ []byte) (Result, error) {
		close(started)
		<-ctx.Done()
		close(exited)
		return Result{Chunks: []Chunk{{Ordinal: 1, Text: "must not publish"}}}, nil
	}
	s.Start(context.Background())
	t.Cleanup(s.Stop)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not claim on startup")
	}
	s.Stop()
	select {
	case <-exited:
	default:
		t.Fatal("extractor not joined")
	}
	var state string
	if err := db.SQL().QueryRow(`SELECT state FROM user_document_extraction_jobs WHERE document_id=?`, doc.ID).Scan(&state); err != nil || state != "running" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	// Simulate an abandoned lease after shutdown; recovery must not require a wakeup.
	if _, err := db.SQL().Exec(`UPDATE user_document_extraction_jobs SET lease_until=0 WHERE document_id=?`, doc.ID); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimUserDocumentExtraction(context.Background(), "restarted")
	if err != nil || job == nil || job.Attempts != 2 {
		t.Fatalf("reclaim: %v %v", job, err)
	}
	NewService(store, nil).process(context.Background(), job)
	docs, err := store.ListUserDocuments(context.Background(), memory.DocumentScope{UserID: "user"})
	if err != nil || len(docs) != 1 || docs[0].Status != "ready" {
		t.Fatalf("recovery: %v %v", docs, err)
	}
}

func TestDocumentServiceCannotPublishLostLease(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE user_document_extraction_jobs SET lease_until=0`,
		`UPDATE user_document_extraction_jobs SET lease_token='replacement'`,
		`UPDATE user_documents SET accepted_at=1,expires_at=2`,
		`DELETE FROM user_documents`,
	} {
		t.Run(mutation, func(t *testing.T) {
			store, db, _ := documentServiceFixture(t)
			job, err := store.ClaimUserDocumentExtraction(context.Background(), "test")
			if err != nil || job == nil {
				t.Fatal(err)
			}
			s := NewService(store, nil)
			s.extract = func(context.Context, string, string, []byte) (Result, error) {
				if _, err := db.SQL().Exec(mutation); err != nil {
					t.Fatal(err)
				}
				return Result{Chunks: []Chunk{{Ordinal: 1, Text: "stale"}}}, nil
			}
			s.process(context.Background(), job)
			var count int
			if err := db.SQL().QueryRow(`SELECT count(*) FROM user_document_chunks`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("published stale data: %d %v", count, err)
			}
		})
	}
}

func TestDocumentServicePublicationRollbackAndRetryTelemetry(t *testing.T) {
	store, db, _ := documentServiceFixture(t)
	ctx := context.Background()
	job, err := store.ClaimUserDocumentExtraction(ctx, "test")
	if err != nil || job == nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`CREATE TRIGGER reject_document_publication BEFORE INSERT ON user_document_chunks WHEN NEW.ordinal=1 BEGIN SELECT RAISE(ABORT,'private-canary'); END`); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := config.NewLogger(config.LevelInfo)
	log.SetOutput(&logs)
	s := NewService(store, log)
	s.extract = func(context.Context, string, string, []byte) (Result, error) {
		return Result{Chunks: []Chunk{{Ordinal: 1, Text: strings.Repeat("a", 20000), Locator: "document", Method: "native"}}}, nil
	}
	s.process(ctx, job)
	var count int
	var token, state string
	if err := db.SQL().QueryRow(`SELECT count(*) FROM user_document_chunks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial publication: count=%d err=%v", count, err)
	}
	if err := db.SQL().QueryRow(`SELECT lease_token,state FROM user_document_extraction_jobs`).Scan(&token, &state); err != nil || state != "running" || token == job.LeaseToken {
		t.Fatalf("lease not rotated/reclaimable: state=%s err=%v", state, err)
	}
	var failed map[string]any
	if err := json.Unmarshal(logs.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	if failed["outcome"] != "reclaimable" || failed["chunk_count"] != float64(0) || failed["text_bytes"] != float64(0) || strings.Contains(logs.String(), "private-canary") {
		t.Fatalf("rollback telemetry: %s", logs.String())
	}
	if _, err := db.SQL().Exec(`DROP TRIGGER reject_document_publication`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`UPDATE user_document_extraction_jobs SET lease_until=0`); err != nil {
		t.Fatal(err)
	}
	job, err = store.ClaimUserDocumentExtraction(ctx, "restarted")
	if err != nil || job == nil {
		t.Fatal(err)
	}
	logs.Reset()
	s.process(ctx, job)
	var succeeded map[string]any
	if err := json.Unmarshal(logs.Bytes(), &succeeded); err != nil {
		t.Fatal(err)
	}
	if succeeded["operation_id"] == failed["operation_id"] || succeeded["outcome"] != "ready" || succeeded["chunk_count"] != float64(2) || succeeded["text_bytes"] != float64(20028) {
		t.Fatalf("retry telemetry: %s", logs.String())
	}
}
