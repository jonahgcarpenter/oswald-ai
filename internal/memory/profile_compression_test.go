package memory

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestProfileCompressionExactRenewalTokenAndPublication(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	turn := appendProfileExchange(t, s, 1, "compression source")
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	scope := ActiveSessionScope{UserID: "alice", SessionID: key, Generation: 1}
	turns, _, err := s.CompressionCandidates(ctx, scope, 0)
	if err != nil || len(turns) != 1 {
		t.Fatal(err)
	}
	claim, err := s.ClaimCompression(ctx, scope, turns, "test-contract")
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := s.RenewCompression(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveCompressionSubmission(ctx, claim); !errors.Is(err, ErrStaleSessionCompactionJobLease) {
		t.Fatal("stale token reserved provider work", err)
	}
	if err := s.ReserveCompressionSubmission(ctx, renewed); err != nil {
		t.Fatal(err)
	}
	artifact := SummaryArtifact{Narrative: "synthetic summary", GenerationModel: "synthetic/model", GeneratorVersion: "synthetic-v1"}
	if err := s.SaveCompressionArtifact(ctx, renewed, artifact); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishProfileSummary(ctx, claim, turns, artifact); !errors.Is(err, ErrStaleSessionCompactionJobLease) {
		t.Fatal("stale token published", err)
	}
	if err := s.PublishProfileSummary(ctx, renewed, turns, artifact); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCompression(ctx, claim, false, false); !errors.Is(err, ErrStaleSessionCompactionJobLease) {
		t.Fatal("stale token released live lease", err)
	}
	if err := s.ReleaseCompression(ctx, renewed, false, false); err != nil {
		t.Fatal(err)
	}
	summary, err := s.LatestSessionSummary(ctx, "alice", key, 1)
	if err != nil || summary.CoveredThroughTurnID != turn.ID {
		t.Fatal("summary not persisted", err)
	}
	if err := s.ResetSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM state_meta WHERE key LIKE 'oswald:%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("reset retained compression metadata", err)
	}
}

func TestProfileCompressionCreditsRefundAndPendingBarrier(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	first := appendProfileExchange(t, s, 1, "first")
	pending := appendProfileExchange(t, s, 1, "pending")
	last := appendProfileExchange(t, s, 1, "last")
	for _, turn := range []StoredSessionTurn{first, last} {
		if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	scope := ActiveSessionScope{UserID: "alice", SessionID: key, Generation: 1}
	turns, _, err := s.CompressionCandidates(ctx, scope, 0)
	if err != nil || len(turns) != 1 || turns[0].ID != first.ID {
		t.Fatal("pending barrier crossed", err)
	}
	claim, err := s.ClaimCompression(ctx, scope, turns, "budget-contract")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveCompressionSubmission(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCompression(ctx, claim, true, false); err != nil {
		t.Fatal(err)
	}
	claim, err = s.ClaimCompression(ctx, scope, turns, "budget-contract")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := s.ReserveCompressionSubmission(ctx, claim); err != nil {
			t.Fatal("preemption did not refund credit", err)
		}
	}
	if err := s.ReserveCompressionSubmission(ctx, claim); !errors.Is(err, ErrModelSubmissionBudgetExhausted) {
		t.Fatal("fifth durable submission allowed", err)
	}
	if err := s.ReleaseCompression(ctx, claim, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCompression(ctx, scope, turns, "budget-contract"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("exhausted range reclaimed", err)
	}
	if err := s.MarkSessionTurnDeliveryFailed(ctx, "alice", pending.ID); err != nil {
		t.Fatal(err)
	}
	turns, _, err = s.CompressionCandidates(ctx, scope, 0)
	if err != nil || len(turns) != 2 {
		t.Fatal("failed delivery retained pending barrier", err)
	}
}

func TestProfileMaintenanceExpiryKeepsGenerationHighwater(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	now := time.Now()
	s.now = func() time.Time { return now }
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	appendProfileExchange(t, s, 1, "private expiry text")
	now = now.Add(2 * time.Hour)
	if err := s.SweepProfile(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 0 {
		t.Fatal("expiry retained messages", err)
	}
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil || session.Generation != 2 {
		t.Fatal("maintenance lost generation highwater", err)
	}
}

func TestProfileCompressionSavedFourthArtifactSurvivesReopen(t *testing.T) {
	s, root := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	turn := appendProfileExchange(t, s, 1, "artifact recovery source")
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	scope := ActiveSessionScope{UserID: "alice", SessionID: key, Generation: 1}
	turns, _, err := s.CompressionCandidates(ctx, scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCompression(ctx, scope, turns, "recovery-contract")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := s.ReserveCompressionSubmission(ctx, claim); err != nil {
			t.Fatal(err)
		}
	}
	artifact := SummaryArtifact{Narrative: "immutable output", GenerationModel: "synthetic/model", GeneratorVersion: "synthetic-v1"}
	if err := s.SaveCompressionArtifact(ctx, claim, artifact); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCompression(ctx, claim, false, false); err != nil {
		t.Fatal(err)
	}
	s.Close()
	reopened, err := NewProfileStore(ctx, root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claim, err = reopened.ClaimCompression(ctx, scope, turns, "recovery-contract")
	if err != nil || claim.Artifact == nil {
		t.Fatal("fourth-credit artifact was lost", err)
	}
	if err := reopened.ReserveCompressionSubmission(ctx, claim); err == nil {
		t.Fatal("saved artifact spent another provider credit")
	}
	if err := reopened.PublishProfileSummary(ctx, claim, turns, *claim.Artifact); err != nil {
		t.Fatal("cached artifact publication failed", err)
	}
	if err := reopened.ReleaseCompression(ctx, claim, false, false); err != nil {
		t.Fatal(err)
	}
}

func TestProfileCompressionPinsCampaignAndCorrectiveFeedback(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second", "third"} {
		turn := appendProfileExchange(t, s, 1, text)
		if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	scope := ActiveSessionScope{UserID: "alice", SessionID: key, Generation: 1}
	turns, _, err := s.CompressionCandidates(ctx, scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCompression(ctx, scope, turns[:1], "campaign-contract")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Target != turns[2].ID {
		t.Fatal("campaign did not pin newest delivered high-water")
	}
	if err := s.ReserveCompressionSubmission(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCompressionCorrection(ctx, claim, "missing_tool_call"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCompression(ctx, claim, false, false); err != nil {
		t.Fatal(err)
	}
	claim, err = s.ClaimCompression(ctx, scope, turns[:1], "campaign-contract")
	if err != nil || claim.CorrectiveCode != "missing_tool_call" {
		t.Fatal("corrective feedback lost", err)
	}
	if err := s.ReserveCompressionSubmission(ctx, claim); err != nil {
		t.Fatal(err)
	}
	artifact := SummaryArtifact{Narrative: "first chunk", GenerationModel: "synthetic/model", GeneratorVersion: "synthetic-v1"}
	if err := s.SaveCompressionArtifact(ctx, claim, artifact); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishProfileSummary(ctx, claim, turns[:1], artifact); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCompression(ctx, claim, false, false); err != nil {
		t.Fatal(err)
	}
	target, err := s.CompressionCampaignTarget(ctx, scope, "campaign-contract")
	if err != nil || target != turns[2].ID {
		t.Fatal("partial checkpoint erased campaign", err)
	}
	if err := s.ResetSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM state_meta WHERE key LIKE 'oswald:%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("reset retained campaign", err)
	}
}

func TestProfileSummaryPublicationRejectsOmittedDeliveredExchange(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first source", "second source"} {
		turn := appendProfileExchange(t, s, 1, text)
		if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	scope := ActiveSessionScope{UserID: "alice", SessionID: key, Generation: 1}
	turns, _, err := s.CompressionCandidates(ctx, scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimCompression(ctx, scope, turns[1:], "omission-contract")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveCompressionSubmission(ctx, claim); err != nil {
		t.Fatal(err)
	}
	artifact := SummaryArtifact{Narrative: "incomplete summary", GenerationModel: "synthetic/model", GeneratorVersion: "synthetic-v1"}
	if err := s.SaveCompressionArtifact(ctx, claim, artifact); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishProfileSummary(ctx, claim, turns[1:], artifact); err == nil {
		t.Fatal("summary omitted delivered source")
	}
	summary, err := s.LatestSessionSummary(ctx, "alice", key, 1)
	if err != nil || summary.ID != 0 {
		t.Fatal("rejected summary was published", err)
	}
	if err := s.ReleaseCompression(ctx, claim, false, false); err != nil {
		t.Fatal(err)
	}
}
