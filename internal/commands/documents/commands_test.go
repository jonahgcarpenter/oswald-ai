package documents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/memorytest"
)

type fixture struct {
	path         string
	h            *handler
	service      *commands.Service
	actor, other identity.Principal
}

func newFixture(t *testing.T, logger ...*config.Logger) fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "documents.db")
	log := config.NewLogger(config.LevelError)
	if len(logger) > 0 {
		log = logger[0]
	}
	store := memorytest.NewStore(t, path, log)
	t.Cleanup(func() { store.Close() })
	users := accounts.NewService(path, store, nil, log)
	t.Cleanup(func() { users.Close() })
	principal := func(external string) identity.Principal {
		id, err := users.EnsureAccount(context.Background(), "homeassistant", external, external)
		if err != nil {
			t.Fatal(err)
		}
		return identity.Principal{CanonicalUserID: id, Gateway: "homeassistant", ExternalID: external, Assurance: identity.AssuranceHomeAssistantToken}
	}
	h := New(users, store).(*handler)
	service, err := commands.NewServiceWithCommands(commands.Command{Handler: h})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{path: path, h: h, service: service, actor: principal("actor"), other: principal("other")}
}

func (f fixture) run(t *testing.T, p identity.Principal, args string) commands.Result {
	t.Helper()
	req := commands.Request{Principal: p, Raw: "/documents " + args}
	targets, err := f.service.ResolveFenceTargets(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// Direct handler fixtures explicitly stand in for the scheduler's fences.
	// broker_test.go exercises real acquisition and delivery lifetimes.
	req.FencedUserIDs = append(targets, p.CanonicalUserID)
	result, err := f.service.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f fixture) upload(t *testing.T, p identity.Principal, count int) []memory.UserDocument {
	t.Helper()
	var docs []memory.UserDocument
	for i := 0; i < count; i++ {
		out, err := f.h.store.AcceptUserDocuments(context.Background(), p.CanonicalUserID, []memory.DocumentUpload{{Filename: fmt.Sprintf("document-%d.txt", i), MediaType: "text/plain", Data: []byte("synthetic document")}})
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, out...)
		job, err := f.h.store.ClaimUserDocumentExtraction(context.Background(), "command-test")
		if err != nil || job == nil {
			t.Fatalf("claim: %v", err)
		}
		if err := f.h.store.CompleteUserDocumentExtraction(context.Background(), job, []memory.DocumentChunk{{Ordinal: 0, Text: "synthetic document", Method: "plain"}}, false); err != nil {
			t.Fatal(err)
		}
	}
	return docs
}

func (f fixture) admin(t *testing.T) {
	t.Helper()
	_, ok, err := f.h.accounts.ClaimBootstrapAdmin(context.Background(), f.actor)
	if err != nil || !ok {
		t.Fatalf("bootstrap: %v, %v", ok, err)
	}
}

func confirmationCode(t *testing.T, result commands.Result) string {
	t.Helper()
	_, code, ok := strings.Cut(result.Text, "/documents confirm ")
	if !ok || len(code) != 32 {
		t.Fatalf("missing confirmation: %q", result.Text)
	}
	return code
}

func TestPrivateScopeAndForgedIdentity(t *testing.T) {
	f := newFixture(t)
	own := f.upload(t, f.actor, 1)[0]
	other := f.upload(t, f.other, 1)[0]
	listed := f.run(t, f.actor, "list")
	if !strings.Contains(listed.Text, own.ID) || strings.Contains(listed.Text, other.ID) {
		t.Fatal(listed.Text)
	}
	u := f.run(t, f.actor, "usage")
	for _, want := range []string{"Your document usage", "Documents: 1 / 50", "Source:", "Extracted text:", "Reserved text:", "Text + reserved:"} {
		if !strings.Contains(u.Text, want) {
			t.Fatalf("missing %q: %s", want, u.Text)
		}
	}
	if result := f.run(t, f.actor, "forget "+other.ID); result.Outcome.Status != "rejected" {
		t.Fatal(result)
	}
	for _, args := range []string{"", "forget", "confirm", "users", "list a b", "usage owner"} {
		if result := f.run(t, f.actor, args); result.Outcome.Status != "rejected" {
			t.Fatalf("%q: %+v", args, result)
		}
	}
	forged := f.actor
	forged.CanonicalUserID = f.other.CanonicalUserID
	for _, args := range []string{"usage", "list", "forget " + other.ID, "forget all"} {
		_, err := f.h.Execute(context.Background(), commands.Request{Principal: forged, Args: strings.Fields(args)})
		if !errors.Is(err, accounts.ErrPrincipalMismatch) {
			t.Fatalf("%q: %v", args, err)
		}
	}
	unauthenticated := f.actor
	unauthenticated.Assurance = identity.AssuranceSelfAsserted
	if _, err := f.h.Execute(context.Background(), commands.Request{Principal: unauthenticated, Args: []string{"list"}}); err == nil {
		t.Fatal("unauthenticated read accepted")
	}
	if result := f.run(t, f.actor, "forget "+own.ID); result.Outcome.AffectedCount != 1 {
		t.Fatal(result)
	}
	if result := f.run(t, f.other, "list"); !strings.Contains(result.Text, other.ID) {
		t.Fatal(result)
	}
}

func TestConfirmationSnapshotBindingExpiryAndReplay(t *testing.T) {
	f := newFixture(t)
	f.upload(t, f.actor, 2)
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	if result := f.run(t, f.other, "confirm "+code); result.Outcome.Status != "rejected" {
		t.Fatal(result)
	}
	later := f.upload(t, f.actor, 1)[0]
	if result := f.run(t, f.actor, "confirm "+code); result.Outcome.AffectedCount != 2 {
		t.Fatal(result)
	}
	if result := f.run(t, f.actor, "list"); !strings.Contains(result.Text, later.ID) {
		t.Fatal(result)
	}
	if result := f.run(t, f.actor, "confirm "+code); result.Outcome.Status != "rejected" {
		t.Fatal(result)
	}
	code = confirmationCode(t, f.run(t, f.actor, "forget all"))
	f.h.now = func() time.Time { return time.Now().Add(confirmationLifetime) }
	if result := f.run(t, f.actor, "confirm "+code); result.Outcome.Status != "rejected" {
		t.Fatal(result)
	}
	if result := f.run(t, f.actor, "list"); !strings.Contains(result.Text, later.ID) {
		t.Fatal(result)
	}
}

func TestConfirmationReplacementAndScopeChanges(t *testing.T) {
	f := newFixture(t)
	f.upload(t, f.actor, 1)
	old := confirmationCode(t, f.run(t, f.actor, "forget all"))
	newCode := confirmationCode(t, f.run(t, f.actor, "forget all"))
	if result := f.run(t, f.actor, "confirm "+old); result.Outcome.Status != "rejected" {
		t.Fatal(result)
	}
	f.admin(t)
	if result := f.run(t, f.actor, "confirm "+newCode); result.Outcome.Status != "rejected" {
		t.Fatal("promotion broadened private snapshot")
	}
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	if _, err := f.h.accounts.SetAdminAs(context.Background(), f.actor, f.other.CanonicalUserID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.accounts.SetAdminAs(context.Background(), f.other, f.actor.CanonicalUserID, false); err != nil {
		t.Fatal(err)
	}
	if result := f.run(t, f.actor, "confirm "+code); result.Outcome.Status != "rejected" {
		t.Fatal("revoked admin confirmed")
	}
	if _, err := f.h.accounts.SetAdminAs(context.Background(), f.other, f.actor.CanonicalUserID, true); err != nil {
		t.Fatal(err)
	}
	if result := f.run(t, f.actor, "confirm "+code); result.Outcome.Status != "rejected" {
		t.Fatal("revoked confirmation survived reuse")
	}
}

func TestAdministratorPaginationBulkAndFences(t *testing.T) {
	f := newFixture(t)
	f.admin(t)
	f.upload(t, f.actor, 31)
	f.upload(t, f.other, 30)
	seen := map[string]bool{}
	after := ""
	for {
		result := f.run(t, f.actor, "list "+after)
		if !strings.Contains(result.Text, "Canonical owner:") {
			t.Fatal(result)
		}
		for _, line := range strings.Split(result.Text, "\n") {
			if id, ok := strings.CutPrefix(line, "ID: "); ok {
				if seen[id] {
					t.Fatal("duplicate page ID")
				}
				seen[id] = true
			}
		}
		_, next, more := strings.Cut(result.Text, "Next: /documents list ")
		if !more {
			break
		}
		after = next
	}
	if len(seen) != 61 {
		t.Fatalf("listed %d", len(seen))
	}
	u := f.run(t, f.actor, "usage")
	if !strings.Contains(u.Text, "Documents: 61 / no global count cap") || !strings.Contains(u.Text, f.other.CanonicalUserID) {
		t.Fatal(u)
	}
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	targets, err := f.service.ResolveFenceTargets(context.Background(), commands.Request{Principal: f.actor, Raw: "/documents confirm " + code})
	if err != nil || len(targets) != 2 || !slices.Contains(targets, f.other.CanonicalUserID) {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
	foreignTargets, err := f.service.ResolveFenceTargets(context.Background(), commands.Request{Principal: f.other, Raw: "/documents confirm " + code})
	if err != nil || len(foreignTargets) != 0 {
		t.Fatalf("unauthorized targets=%v err=%v", foreignTargets, err)
	}
	later := f.upload(t, f.other, 1)[0]
	result := f.run(t, f.actor, "confirm "+code)
	if result.Outcome.Status != "ok" || result.Outcome.AffectedCount != 61 {
		t.Fatal(result)
	}
	if result := f.run(t, f.actor, "list"); !strings.Contains(result.Text, later.ID) {
		t.Fatal(result)
	}
	if result := f.run(t, f.actor, "forget "+later.ID); result.Outcome.AffectedCount != 1 {
		t.Fatal(result)
	}
}

func TestBulkFailureReportsCommittedBatches(t *testing.T) {
	f := newFixture(t)
	f.admin(t)
	f.upload(t, f.actor, 1)
	f.upload(t, f.other, 1)
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	c := f.h.pending[sha256.Sum256([]byte(code))]
	owners := []string{f.actor.CanonicalUserID, f.other.CanonicalUserID}
	slices.Sort(owners)
	if _, err := f.h.store.DeleteUserDocuments(context.Background(), memory.DocumentScope{UserID: owners[1]}, c.owners[owners[1]]); err != nil {
		t.Fatal(err)
	}
	result := f.run(t, f.actor, "confirm "+code)
	if result.Outcome.Status != "error" || result.Outcome.AffectedCount != 1 || !result.Outcome.IsChanged || !strings.Contains(result.Text, "NOT rolled back") {
		t.Fatal(result)
	}
	if result := f.run(t, f.actor, "confirm "+code); result.Outcome.Status != "rejected" {
		t.Fatal(result)
	}
}

func TestUsageOwnerPaginationAndPendingCapacity(t *testing.T) {
	f := newFixture(t)
	f.admin(t)
	for i := 0; i < 10; i++ {
		if _, err := f.h.accounts.EnsureAccount(context.Background(), "homeassistant", fmt.Sprintf("extra-%d", i), "Synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	first := f.run(t, f.actor, "usage")
	_, next, ok := strings.Cut(first.Text, "Next: /documents usage ")
	if !ok {
		t.Fatal(first)
	}
	second := f.run(t, f.actor, "usage "+strings.TrimSpace(next))
	if strings.Contains(second.Text, "Next:") || strings.Count(second.Text, ": documents=") != 2 {
		t.Fatal(second)
	}
	f.upload(t, f.actor, 1)
	for i := 0; i < 100; i++ {
		f.h.pending[sha256.Sum256([]byte(fmt.Sprint(i)))] = confirmation{principal: f.other, expires: time.Now().Add(time.Hour), count: 1}
	}
	if result := f.run(t, f.actor, "forget all"); result.Outcome.Status != "rejected" {
		t.Fatal(result)
	}
}

func TestConcurrentConfirmationIsOneUse(t *testing.T) {
	f := newFixture(t)
	f.upload(t, f.actor, 1)
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	start := make(chan struct{})
	results := make(chan commands.Result, 2)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			result, err := f.service.Execute(context.Background(), commands.Request{Principal: f.actor, Raw: "/documents confirm " + code, FencedUserIDs: []string{f.actor.CanonicalUserID}})
			results <- result
			errors <- err
		}()
	}
	close(start)
	deleted, rejected := 0, 0
	for range 2 {
		result := <-results
		deleted += result.Outcome.AffectedCount
		if result.Outcome.Status == "rejected" {
			rejected++
		}
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if deleted != 1 || rejected != 1 {
		t.Fatalf("deleted=%d rejected=%d", deleted, rejected)
	}
}

func TestDocumentCommandLogsDoNotExposeConfirmations(t *testing.T) {
	for _, level := range []config.Level{config.LevelInfo, config.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			var logs bytes.Buffer
			logger := config.NewLogger(level)
			logger.SetOutput(&logs)
			f := newFixture(t, logger)
			doc := f.upload(t, f.actor, 1)[0]
			logs.Reset()
			code := confirmationCode(t, f.run(t, f.actor, "forget all"))
			f.run(t, f.actor, "usage")
			f.run(t, f.actor, "list")
			f.run(t, f.actor, "confirm "+code)
			for _, private := range []string{code, doc.ID, doc.Filename, "synthetic document"} {
				if strings.Contains(logs.String(), private) {
					t.Fatal("private document data appeared in logs")
				}
			}
			if strings.Count(logs.String(), `"operation":"document_delete"`) != 1 || !strings.Contains(logs.String(), `"deleted_count":1`) {
				t.Fatal("missing unique committed deletion measurement")
			}
		})
	}
}

func TestManagementIncludesExpiredDocumentsWithoutChangingToolScope(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint(admin), func(t *testing.T) {
			f := newFixture(t)
			if admin {
				f.admin(t)
			}
			own := f.upload(t, f.actor, 1)[0]
			other := f.upload(t, f.other, 1)[0]
			db, err := database.Open(f.path, config.NewLogger(config.LevelError))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// Deliberate expired-metadata fixture; ordinary writes use admission.
			if _, err := db.SQL().Exec(`UPDATE user_documents SET accepted_at=?,expires_at=?`, time.Now().Add(-31*24*time.Hour).UnixMilli(), time.Now().Add(-time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			result := f.run(t, f.actor, "list")
			if !strings.Contains(result.Text, own.ID) || !strings.Contains(result.Text, "Expired: retained") || strings.Contains(result.Text, other.ID) != admin {
				t.Fatal(result)
			}
			live, err := f.h.store.ListUserDocuments(context.Background(), memory.DocumentScope{UserID: f.actor.CanonicalUserID})
			if err != nil || len(live) != 0 {
				t.Fatalf("tool listing=%v err=%v", live, err)
			}
			if _, err := f.h.store.ReadUserDocument(context.Background(), f.actor.CanonicalUserID, own.ID, 0, 1); !errors.Is(err, memory.ErrDocumentNotFound) {
				t.Fatalf("expired tool read: %v", err)
			}
			count := 1
			if admin {
				count = 2
			}
			if result := f.run(t, f.actor, "usage"); !strings.Contains(result.Text, fmt.Sprintf("Expired retained documents: %d", count)) {
				t.Fatal(result)
			}
			code := confirmationCode(t, f.run(t, f.actor, "forget all"))
			if result := f.run(t, f.actor, "confirm "+code); result.Outcome.AffectedCount != count {
				t.Fatal(result)
			}
			if result := f.run(t, f.actor, "list"); strings.Contains(result.Text, "ID: ") {
				t.Fatal(result)
			}
		})
	}
}

func TestUsageIncludesReservationsAndLiveBacklogWithoutDoubleCounting(t *testing.T) {
	f := newFixture(t)
	docs := f.upload(t, f.actor, 6)
	db, err := database.Open(f.path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, status := range []string{"queued", "extracting", "failed", "ready", "partial"} {
		if _, err := db.SQL().Exec(`UPDATE user_documents SET status=? WHERE id=?`, status, docs[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().Exec(`UPDATE user_documents SET accepted_at=?,expires_at=? WHERE id=?`, time.Now().Add(-31*24*time.Hour).UnixMilli(), time.Now().Add(-time.Hour).UnixMilli(), docs[5].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.store.ReserveUserDocumentUpload(context.Background(), f.actor.CanonicalUserID, 2, 123); err != nil {
		t.Fatal(err)
	}
	queued, err := f.h.store.AcceptUserDocuments(context.Background(), f.actor.CanonicalUserID, []memory.DocumentUpload{{Filename: "queued.txt", MediaType: "text/plain", Data: []byte("queued")}})
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued=%v err=%v", queued, err)
	}
	result := f.run(t, f.actor, "usage")
	for _, want := range []string{
		"Documents: 7 / 50", "Reserved document slots: 2", "Documents + reserved slots: 9 / 50",
		"Reserved source: 123", "Source + reserved: 237 / 262144000",
		"Reserved text: 3145728 (extraction: 1048576; upload: 2097152)",
		"Text + reserved: 3145866 / 26214400", "Live upload reservations: 1", "Expired retained documents: 1",
		"Live statuses: queued=2 running=1 failed=1 ready=1 partial=1",
	} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("missing %q in %s", want, result.Text)
		}
	}
	f.admin(t)
	global := f.run(t, f.actor, "usage")
	if !strings.Contains(global.Text, "upload_reservations=1 expired=1 live: queued=2 running=1 failed=1 ready=1 partial=1") {
		t.Fatal(global)
	}
}

type fakeStorageStats struct {
	stats memory.DocumentStorageStats
	err   error
	calls int
}

func (f *fakeStorageStats) UserDocumentStorageStats(context.Context) (memory.DocumentStorageStats, error) {
	f.calls++
	return f.stats, f.err
}

func TestStorageStatisticsAreAdministratorOnlyAndUnknownIsNotZero(t *testing.T) {
	f := newFixture(t)
	disk := &fakeStorageStats{stats: memory.DocumentStorageStats{DatabaseBytes: 123, WALBytes: 456, FreeBytes: 789, DiskKnown: true}}
	f.h.disk = disk
	result := f.run(t, f.actor, "usage")
	if disk.calls != 0 || strings.Contains(result.Text, "Server database:") || strings.Contains(result.Text, "Filesystem") {
		t.Fatal(result)
	}
	f.admin(t)
	result = f.run(t, f.actor, "usage")
	if disk.calls != 1 || !strings.Contains(result.Text, "Server database: 123 bytes; WAL: 456 bytes") || !strings.Contains(result.Text, "Filesystem available space: 789 bytes") {
		t.Fatal(result)
	}
	disk.stats.DiskKnown = false
	if result := f.run(t, f.actor, "usage"); !strings.Contains(result.Text, "Filesystem available space: unknown.") || strings.Contains(result.Text, "789") {
		t.Fatal(result)
	}
	disk.err = errors.New("private-storage-error")
	if result := f.run(t, f.actor, "usage"); !strings.Contains(result.Text, "Server storage statistics: unavailable.") || strings.Contains(result.Text, "private-storage-error") || strings.Contains(result.Text, "Server database:") {
		t.Fatal(result)
	}
	f.h.disk = nil
	if result := f.run(t, f.actor, "usage"); !strings.Contains(result.Text, "Server storage statistics: unavailable.") {
		t.Fatal(result)
	}
}

func TestUnknownIDInFirstBatchRollsBackWholeBatch(t *testing.T) {
	f := newFixture(t)
	f.admin(t)
	f.upload(t, f.actor, 3)
	f.upload(t, f.other, 2)
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	c := f.h.pending[sha256.Sum256([]byte(code))]
	owners := []string{f.actor.CanonicalUserID, f.other.CanonicalUserID}
	slices.Sort(owners)
	ids := c.owners[owners[0]]
	if _, err := f.h.store.DeleteUserDocuments(context.Background(), memory.DocumentScope{UserID: owners[0]}, ids[len(ids)-1:]); err != nil {
		t.Fatal(err)
	}
	result := f.run(t, f.actor, "confirm "+code)
	if result.Outcome.Status != "error" || result.Outcome.AffectedCount != 0 || result.Outcome.IsChanged || !strings.Contains(result.Text, "Deleted 0 of 5") {
		t.Fatal(result)
	}
	u, err := f.h.store.UserDocumentUsage(context.Background(), memory.DocumentScope{Global: true})
	if err != nil || u.DocumentCount != 4 {
		t.Fatalf("usage=%+v err=%v", u, err)
	}
}

func TestOwnershipMoveAfterSnapshotDoesNotDeleteFromNewOwner(t *testing.T) {
	f := newFixture(t)
	f.admin(t)
	doc := f.upload(t, f.other, 1)[0]
	code := confirmationCode(t, f.run(t, f.actor, "forget all"))
	db, err := database.Open(f.path, config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Simulate an ownership move while a confirmation is waiting, without
	// changing the initiating administrator's own authentication binding.
	if _, err := db.SQL().Exec(`UPDATE user_documents SET canonical_user_id=? WHERE id=?`, f.actor.CanonicalUserID, doc.ID); err != nil {
		t.Fatal(err)
	}
	result := f.run(t, f.actor, "confirm "+code)
	if result.Outcome.Status != "error" || result.Outcome.AffectedCount != 0 {
		t.Fatal(result)
	}
	if result := f.run(t, f.actor, "list"); !strings.Contains(result.Text, doc.ID) {
		t.Fatal(result)
	}
}
