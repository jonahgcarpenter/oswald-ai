package usermemory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

func TestFormationCandidatePublicationIsIdempotentAndTraceable(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	output := evaluatedFormationCandidate(t, "I use Go for project Atlas", "I use Go for project Atlas", "The user uses Go for project Atlas.", memoryformation.CategoryProjects)
	proposal := CandidateProposal{Output: output, IdempotencyKey: "candidate-1", Source: FormationSource{RequestID: "req-1", SessionID: "session-1", ExtractorVersion: FormationExtractorVersion}}
	candidate, created, err := store.ProposeCandidate(ctx, "user-1", proposal)
	if err != nil || !created || candidate.State != "approved" {
		t.Fatalf("propose candidate=%+v created=%v err=%v", candidate, created, err)
	}
	duplicate, created, err := store.ProposeCandidate(ctx, "user-1", proposal)
	if err != nil || created || duplicate.ID != candidate.ID {
		t.Fatalf("idempotent proposal=%+v created=%v err=%v", duplicate, created, err)
	}
	memory, err := store.PublishCandidate(ctx, "user-1", candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := store.PublishCandidate(ctx, "user-1", candidate.ID)
	if err != nil || again.ID != memory.ID {
		t.Fatalf("idempotent publication=%+v err=%v", again, err)
	}
	var approval, provenance, authority, mode, sensitivity string
	var candidateID int64
	if err := store.sql.QueryRow(`SELECT approval_state, provenance_type, source_authority, formation_mode, sensitivity, candidate_id FROM memory_entries WHERE id = ?`, memory.ID).Scan(&approval, &provenance, &authority, &mode, &sensitivity, &candidateID); err != nil {
		t.Fatal(err)
	}
	if approval != "approved" || provenance != "user_statement" || authority != "user_direct" || mode != "automatic_extraction" || sensitivity != "low" || candidateID != candidate.ID {
		t.Fatalf("unexpected formation metadata: approval=%s provenance=%s authority=%s mode=%s sensitivity=%s candidate=%d", approval, provenance, authority, mode, sensitivity, candidateID)
	}
	if _, err := store.ResolveSessionProfile(ctx, "user-1", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"memory_evidence": 2, "memory_formation_audit": 2, "sessions": 1} {
		var got int
		if err := store.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s count=%d want=%d", table, got, want)
		}
	}
}

func TestFormationPublicationRollbackKeepsOldMemoryActive(t *testing.T) {
	stages := []string{"validated", "canonical_written", "vector_written", "supersession_written", "audit_written", "profile_written", "candidate_published"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
			defer store.Close() // nolint:errcheck
			seedAccountUsers(t, store, "user-1")
			old, err := store.SaveMemory(context.Background(), "user-1", SaveRequest{Scope: ScopeLongTerm, Category: "environment", Statement: "The user lives in Boston.", Evidence: "legacy", Confidence: 1, Importance: 4})
			if err != nil {
				t.Fatal(err)
			}
			output := evaluatedFormationCandidate(t, "I moved to Porto", "I moved to Porto", "The user lives in Porto.", memoryformation.CategoryEnvironment)
			candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "replace", Source: FormationSource{RequestID: "req"}, SupersedesStatement: old.Statement})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.sql.Exec(`UPDATE memory_candidates SET state = 'approved' WHERE id = ?`, candidate.ID); err != nil {
				t.Fatal(err)
			}
			store.formationFailpoint = func(current string) error {
				if current == stage {
					return fmt.Errorf("injected %s failure", stage)
				}
				return nil
			}
			if _, err := store.PublishCandidate(context.Background(), "user-1", candidate.ID); err == nil {
				t.Fatal("expected publication failure")
			}
			store.formationFailpoint = nil
			var status string
			if err := store.sql.QueryRow(`SELECT status FROM memory_entries WHERE id = ?`, old.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "active" {
				t.Fatalf("old status=%s want active", status)
			}
			var replacements int
			if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'user-1' AND statement = 'The user lives in Porto.'`).Scan(&replacements); err != nil {
				t.Fatal(err)
			}
			if replacements != 0 {
				t.Fatalf("replacement count=%d after rollback", replacements)
			}
			loaded, err := store.LoadCandidate(context.Background(), "user-1", candidate.ID)
			if err != nil || loaded.PublishedMemoryID != 0 {
				t.Fatalf("candidate after rollback=%+v err=%v", loaded, err)
			}
		})
	}
}

func TestApprovedCandidatePublishesWhileEmbeddingUnavailable(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), fixedRecallEmbedder{err: errors.New("embedding offline")}, "embed-model", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
	candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "embedding-recovery"})
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishCandidate(context.Background(), "user-1", candidate.ID)
	if err != nil || published.ID == 0 {
		t.Fatalf("canonical publication failed during embedding outage: %+v %v", published, err)
	}
	loaded, err := store.LoadCandidate(context.Background(), "user-1", candidate.ID)
	if err != nil || loaded.State != "approved" || loaded.PublishedMemoryID != published.ID {
		t.Fatalf("candidate after outage=%+v err=%v", loaded, err)
	}
	var changes int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'derived_index' AND entity_kind = 'memory' AND entity_id = ?`, published.ID).Scan(&changes); err != nil || changes != 1 {
		t.Fatalf("derived changes=%d err=%v", changes, err)
	}
}

func TestDirectCorrectionSupersedesOldClaimSlotMemory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	oldOutput := evaluatedClaimCandidate(t, "I live in Boston.", "The user lives in Boston.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.9, "environment.home_city", "Boston")
	oldCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: oldOutput, IdempotencyKey: "old-city"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.PublishCandidate(ctx, "user-1", oldCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := evaluatedClaimCandidate(t, "I live in Porto.", "The user lives in Porto.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.95, "environment.home_city", "Porto")
	candidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: output, IdempotencyKey: "direct-correction", SupersedesStatement: old.Statement})
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishCandidate(ctx, "user-1", candidate.ID)
	if err != nil || published.ID == old.ID || published.ClaimSlot != "environment.home_city" || published.ClaimValue != "porto" {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	var status string
	if err := store.sql.QueryRow(`SELECT status FROM memory_entries WHERE id = ?`, old.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" {
		t.Fatalf("old status=%s", status)
	}
}

func TestHighConfidenceConflictingClaimSlotAutomaticallySupersedes(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	oldOutput := evaluatedClaimCandidate(t, "I live in Boston.", "The user lives in Boston.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.9, "environment.home_city", "Boston")
	oldCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: oldOutput, IdempotencyKey: "automatic-old-city"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.PublishCandidate(ctx, "user-1", oldCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	output := evaluatedClaimCandidate(t, "I live in Porto.", "The user lives in Porto.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.95, "environment.home_city", "Porto")
	candidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: output, IdempotencyKey: "automatic-contradiction"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != "approved" || candidate.SupersedesMemoryID != old.ID || candidate.SupersedesStatement != old.Statement {
		t.Fatalf("contradiction candidate=%+v", candidate)
	}
	newMemory, err := store.PublishCandidate(ctx, "user-1", candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	var oldStatus string
	if err := store.sql.QueryRow(`SELECT status FROM memory_entries WHERE id = ?`, old.ID).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListMemories("user-1", "", "environment", 10)
	if err != nil || oldStatus != "superseded" || len(active) != 1 || active[0].ID != newMemory.ID {
		t.Fatalf("old_status=%s active=%+v new=%+v err=%v", oldStatus, active, newMemory, err)
	}
}

func TestFormationCanReactivateInactiveExactStatement(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
	first, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.PublishCandidate(context.Background(), "user-1", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET status = 'expired' WHERE id = ?`, memory.ID); err != nil {
		t.Fatal(err)
	}
	second, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "second"})
	if err != nil {
		t.Fatal(err)
	}
	reactivated, err := store.PublishCandidate(context.Background(), "user-1", second.ID)
	if err != nil || reactivated.ID != memory.ID || reactivated.Status != "active" {
		t.Fatalf("reactivated=%+v err=%v", reactivated, err)
	}
}

func TestFormationCannotReactivateForgottenExactStatement(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
	first, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "first-forgotten"})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.PublishCandidate(context.Background(), "user-1", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForgetMemory(context.Background(), "user-1", hashText("actor"), memory.ID, "forget-request", time.Now().UTC(), config.RetentionPolicy{ForgottenContentGrace: time.Hour}); err != nil {
		t.Fatal(err)
	}
	second, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "second-forgotten"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishCandidate(context.Background(), "user-1", second.ID); err == nil {
		t.Fatal("forgotten memory was republished")
	}
	var status string
	if err := store.sql.QueryRow(`SELECT status FROM memory_entries WHERE id = ?`, memory.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "forgotten" {
		t.Fatalf("forgotten memory status = %q", status)
	}
}

func TestExpiredPublishedMemoryErasesCandidateEvidence(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
	candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "expire-published"})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := store.PublishCandidate(context.Background(), "user-1", candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE memory_entries SET expires_at = ? WHERE id = ?`, formatTime(now.Add(-time.Minute)), memory.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CleanupExpiredSessions(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCandidate(context.Background(), "user-1", candidate.ID)
	if err != nil || loaded.Statement != "" || loaded.Evidence != "" || loaded.ClaimSlot != "" || loaded.ClaimValue != "" || loaded.State != "rejected" {
		t.Fatalf("expired candidate=%+v err=%v", loaded, err)
	}
	canonical, err := store.EntryByID(memory.ID)
	if err != nil || canonical.Statement != "" || canonical.Evidence != "" || canonical.Status != "expired" {
		t.Fatalf("expired canonical memory=%+v err=%v", canonical, err)
	}
	var profileFacts int
	if err := store.sql.QueryRow(`SELECT count(*) FROM sessions, json_each(sessions.source_memory_ids) source WHERE CAST(source.value AS INTEGER) = ?`, memory.ID).Scan(&profileFacts); err != nil {
		t.Fatal(err)
	}
	if profileFacts != 0 {
		t.Fatalf("expired memory remains in %d profile snapshots", profileFacts)
	}
}

func TestClaimKeyReinforcementKeepsMemoryIDAndAggregatesConfidence(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	firstOutput := evaluatedClaimCandidate(t, "I use Go for Atlas.", "The user uses Go for Atlas.", memoryformation.CategoryProjects, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.6, "projects.atlas.language", "Go")
	firstCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: firstOutput, IdempotencyKey: "reinforcement-1"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.PublishCandidate(ctx, "user-1", firstCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondOutput := evaluatedClaimCandidate(t, "I still use Go for Atlas.", "The user still uses Go for Atlas.", memoryformation.CategoryProjects, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.5, "projects.atlas.language", "Go")
	if secondOutput.ClaimKey != firstOutput.ClaimKey {
		t.Fatalf("claim keys differ: %q != %q", secondOutput.ClaimKey, firstOutput.ClaimKey)
	}
	secondCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: secondOutput, IdempotencyKey: "reinforcement-2"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PublishCandidate(ctx, "user-1", secondCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("reinforced memory ID=%d want=%d", second.ID, first.ID)
	}
	if diff := second.Confidence - 0.8; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("noisy-OR confidence=%v want=0.8", second.Confidence)
	}
	if second.EvidenceCount != 2 {
		t.Fatalf("evidence_count=%d want=2", second.EvidenceCount)
	}
	var attachedEvidence int
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_evidence WHERE memory_id = ?`, first.ID).Scan(&attachedEvidence); err != nil || attachedEvidence != 2 {
		t.Fatalf("attached evidence=%d want=2 err=%v", attachedEvidence, err)
	}
}

func TestDirectEvidenceUpgradesInferenceInPlaceAndRetainsSensitivity(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	inferredOutput := evaluatedClaimCandidate(t, "Concise replies seem preferable.", "The user prefers concise replies.", memoryformation.CategoryCommunicationPreferences, memoryformation.ProvenanceModelInference, memoryformation.SensitivityHighImpactInteraction, 0.6, "communication.reply_style", "concise")
	inferredCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: inferredOutput, IdempotencyKey: "inferred-style"})
	if err != nil {
		t.Fatal(err)
	}
	inferred, err := store.PublishCandidate(ctx, "user-1", inferredCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	var profileApproved bool
	if err := store.sql.QueryRow(`SELECT profile_approved != 0 FROM memory_entries WHERE id = ?`, inferred.ID).Scan(&profileApproved); err != nil || profileApproved {
		t.Fatalf("inferred profile_approved=%v err=%v", profileApproved, err)
	}
	directOutput := evaluatedClaimCandidate(t, "I prefer concise replies.", "The user prefers concise replies.", memoryformation.CategoryCommunicationPreferences, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.6, "communication.reply_style", "concise")
	directCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: directOutput, IdempotencyKey: "direct-style"})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := store.PublishCandidate(ctx, "user-1", directCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if direct.ID != inferred.ID || direct.SourceAuthority != string(memoryformation.AuthorityUserDirect) || direct.ProvenanceType != string(memoryformation.ProvenanceUserStatement) {
		t.Fatalf("upgraded memory=%+v inferred ID=%d", direct, inferred.ID)
	}
	if direct.Sensitivity != string(memoryformation.SensitivityHighImpactInteraction) || direct.EvidenceCount != 2 {
		t.Fatalf("reinforced sensitivity/evidence=%s/%d", direct.Sensitivity, direct.EvidenceCount)
	}
	if diff := direct.Confidence - 0.84; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("reinforced confidence=%v want=0.84", direct.Confidence)
	}
	if _, err := store.ResolveSessionProfile(ctx, "user-1", "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	var profileFacts int
	if err := store.sql.QueryRow(`SELECT count(*) FROM sessions, json_each(sessions.source_memory_ids) source WHERE CAST(source.value AS INTEGER) = ?`, direct.ID).Scan(&profileFacts); err != nil || profileFacts != 1 {
		t.Fatalf("profile facts=%d want=1 err=%v", profileFacts, err)
	}
}

func TestLowConfidenceCandidateStaysProposedAndCannotPublish(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	output := evaluatedClaimCandidate(t, "Concise replies might be preferable.", "The user might prefer concise replies.", memoryformation.CategoryCommunicationPreferences, memoryformation.ProvenanceModelInference, memoryformation.SensitivityLow, 0.34, "communication.reply_style", "concise")
	candidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: output, IdempotencyKey: "low-confidence"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.State != "proposed" || candidate.PolicyDecision != string(memoryformation.DecisionProposed) {
		t.Fatalf("low-confidence candidate=%+v", candidate)
	}
	if _, err := store.PublishCandidate(ctx, "user-1", candidate.ID); err == nil {
		t.Fatal("low-confidence proposed candidate published")
	}
	var memories int
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_entries WHERE canonical_user_id = 'user-1'`).Scan(&memories); err != nil || memories != 0 {
		t.Fatalf("published memories=%d want=0 err=%v", memories, err)
	}
}

func TestCorrelatedInferenceEvidenceIsDiscounted(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	output := evaluatedClaimCandidate(t, "Which pacman file manager is best?", "The user may use a pacman-based Linux environment.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceModelInference, memoryformation.SensitivityLow, 0.5, "environment.linux_distribution", "arch_family")
	firstCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: output, IdempotencyKey: "same-session-1", Source: FormationSource{SessionID: "session-a"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.PublishCandidate(ctx, "user-1", firstCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: output, IdempotencyKey: "same-session-2", Source: FormationSource{SessionID: "session-a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PublishCandidate(ctx, "user-1", secondCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("correlated evidence created memory %d, want %d", second.ID, first.ID)
	}
	if diff := second.Confidence - 0.5625; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("discounted confidence=%v want=0.5625", second.Confidence)
	}
}

func TestWeakInferenceCannotSupersedeDirectClaim(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	ctx := context.Background()
	directOutput := evaluatedClaimCandidate(t, "I use Fedora.", "The user uses Fedora.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.95, "environment.linux_distribution", "fedora")
	directCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: directOutput, IdempotencyKey: "direct-fedora"})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := store.PublishCandidate(ctx, "user-1", directCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	inferredOutput := evaluatedClaimCandidate(t, "Which pacman file manager is best?", "The user may use a pacman-based Linux environment.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceModelInference, memoryformation.SensitivityLow, 0.55, "environment.linux_distribution", "arch_family")
	inferredCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: inferredOutput, IdempotencyKey: "inferred-arch"})
	if err != nil {
		t.Fatal(err)
	}
	if inferredCandidate.SupersedesMemoryID != 0 || inferredCandidate.State != "proposed" {
		t.Fatalf("weak inference targeted direct memory: %+v", inferredCandidate)
	}
	if _, err := store.PublishCandidate(ctx, "user-1", inferredCandidate.ID); err == nil {
		t.Fatal("weak conflicting inference published")
	}
	loaded, err := store.EntryByID(direct.ID)
	if err != nil || loaded.Status != "active" {
		t.Fatalf("direct memory status=%q err=%v", loaded.Status, err)
	}
	explicitTargetCandidate, _, err := store.ProposeCandidate(ctx, "user-1", CandidateProposal{Output: inferredOutput, IdempotencyKey: "inferred-explicit-target", SupersedesStatement: direct.Statement})
	if err != nil {
		t.Fatal(err)
	}
	if explicitTargetCandidate.State != "proposed" || explicitTargetCandidate.SupersedesMemoryID != 0 {
		t.Fatalf("weak extractor supersession bypassed authority: %+v", explicitTargetCandidate)
	}
}

func TestForgetAllErasesUnpublishedCandidateContent(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	in := memoryformation.CandidateInput{SourceUserText: "My phone is 555-0100", Statement: "The user's phone is 555-0100.", Evidence: "My phone is 555-0100", Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect, Sensitivity: memoryformation.SensitivityIdentityOrContact, Mode: memoryformation.ModeAutomaticExtraction, Scope: memoryformation.ScopeLongTerm, Category: memoryformation.CategoryIdentity, Context: memoryformation.ContextDirectAssertion, Confidence: 0.95, Importance: 4}
	output, err := memoryformation.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "unpublished"})
	if err != nil {
		t.Fatal(err)
	}
	turnID := seedFormationTurn(t, store, "user-1", "session", "sensitive source")
	if _, err := store.EnqueueFormationJob(context.Background(), FormationSource{TurnID: turnID, SessionID: "session", ExtractorVersion: FormationExtractorVersion}, "user-1"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFormationJobArtifact(context.Background(), job, `[{"evidence":"sensitive source"}]`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Forget("user-1", "all", ""); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCandidate(context.Background(), "user-1", candidate.ID)
	if err != nil || loaded.Statement != "" || loaded.Evidence != "" || loaded.State != "rejected" {
		t.Fatalf("forgotten candidate=%+v err=%v", loaded, err)
	}
	artifact, err := store.FormationJobArtifact(context.Background(), job)
	if err != nil || artifact != "" {
		t.Fatalf("forgotten formation artifact=%q err=%v", artifact, err)
	}
}

func TestFormationJobLeaseRetryAndReplay(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	turnID := seedFormationTurn(t, store, "user-1", "session-1", "Remember this")
	source := FormationSource{RequestID: "req", SessionID: "session-1", SessionGeneration: 1, TurnID: turnID, Model: "model", ExtractorVersion: FormationExtractorVersion}
	firstID, err := store.EnqueueFormationJob(context.Background(), source, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := store.EnqueueFormationJob(context.Background(), source, "user-1")
	if err != nil || firstID != secondID {
		t.Fatalf("replayed job id=%d want=%d err=%v", secondID, firstID, err)
	}
	job, err := store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.ID != firstID || job.AttemptCount != 1 {
		t.Fatalf("claimed job=%+v err=%v", job, err)
	}
	if err := store.RetryFormationJob(context.Background(), job, "extractor failed", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET available_at = ? WHERE id = ? AND job_kind = 'memory_formation'`, formatTime(time.Now().Add(-time.Second)), firstID); err != nil {
		t.Fatal(err)
	}
	job, err = store.ClaimFormationJob(context.Background(), time.Minute)
	if err != nil || job.AttemptCount != 2 {
		t.Fatalf("retried job=%+v err=%v", job, err)
	}
	if err := store.CompleteFormationJob(context.Background(), job, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimFormationJob(context.Background(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim after completion error=%v", err)
	}
}

func TestFormationReconciliationRequiresDeliveryMarkerAndRedrivesDeadJobs(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	deliveredTurn := seedFormationTurn(t, store, "user-1", "session", "delivered")
	_ = seedFormationTurn(t, store, "user-1", "session", "not delivered")
	if err := store.MarkFormationEligible(context.Background(), "user-1", deliveredTurn); err != nil {
		t.Fatal(err)
	}
	created, err := store.ReconcileFormationJobs(context.Background(), "model", FormationExtractorVersion)
	if err != nil || created != 1 {
		t.Fatalf("reconciled=%d err=%v", created, err)
	}
	created, err = store.ReconcileFormationJobs(context.Background(), "model", FormationExtractorVersion)
	if err != nil || created != 0 {
		t.Fatalf("second reconciliation=%d err=%v", created, err)
	}
	var jobID int64
	if err := store.sql.QueryRow(`SELECT id FROM durable_jobs WHERE job_kind = 'memory_formation' AND source_turn_id = ?`, deliveredTurn).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'dead', attempt_count = 5, completed_at = ?, last_error_code = 'transient_provider', updated_at = ? WHERE id = ? AND job_kind = 'memory_formation'`, formatTime(time.Now().Add(-time.Hour)), formatTime(time.Now().Add(-time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
	redriven, err := store.RedriveDeadFormationJobs(context.Background(), 5*time.Minute)
	if err != nil || redriven != 1 {
		t.Fatalf("redriven=%d err=%v", redriven, err)
	}
	state, err := store.FormationJobState(context.Background(), "user-1", jobID)
	if err != nil || state != "retry" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	for cycle := 1; cycle < 3; cycle++ {
		if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'dead', updated_at = ? WHERE id = ? AND job_kind = 'memory_formation'`, formatTime(time.Now().Add(-24*time.Hour)), jobID); err != nil {
			t.Fatal(err)
		}
		redriven, err = store.RedriveDeadFormationJobs(context.Background(), time.Minute)
		if err != nil || redriven != 1 {
			t.Fatalf("redrive cycle %d count=%d err=%v", cycle+1, redriven, err)
		}
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'dead', updated_at = ? WHERE id = ? AND job_kind = 'memory_formation'`, formatTime(time.Now().Add(-24*time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
	redriven, err = store.RedriveDeadFormationJobs(context.Background(), time.Minute)
	if err != nil || redriven != 0 {
		t.Fatalf("quarantined redrive count=%d err=%v", redriven, err)
	}
	if _, err := store.sql.Exec(`UPDATE durable_jobs SET state = 'dead', redrive_count = 0, last_error_code = 'invalid_output', updated_at = ? WHERE id = ? AND job_kind = 'memory_formation'`, formatTime(time.Now().Add(-24*time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
	redriven, err = store.RedriveDeadFormationJobs(context.Background(), time.Minute)
	if err != nil || redriven != 0 {
		t.Fatalf("permanent failure redrive count=%d err=%v", redriven, err)
	}
}

func TestApprovedPublicationRecoverySurvivesSourceTurnExpiry(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user-1")
	turnID := seedFormationTurn(t, store, "user-1", "session", "I use Go for Atlas")
	if err := store.MarkFormationEligible(context.Background(), "user-1", turnID); err != nil {
		t.Fatal(err)
	}
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
	candidate, _, err := store.ProposeCandidate(context.Background(), "user-1", CandidateProposal{Output: output, IdempotencyKey: "eligible-recovery", Source: FormationSource{TurnID: turnID, SessionID: "session"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE memory_candidates SET updated_at = ? WHERE id = ?; DELETE FROM session_turns WHERE id = ?`, formatTime(time.Now().Add(-time.Hour)), candidate.ID, turnID); err != nil {
		t.Fatal(err)
	}
	retryable, err := store.ApprovedUnpublishedCandidates(context.Background(), 10)
	if err != nil || len(retryable) != 1 || retryable[0].ID != candidate.ID || !retryable[0].FormationEligibleAt.After(time.Time{}) {
		t.Fatalf("retryable after turn expiry=%+v err=%v", retryable, err)
	}
}

func TestMergeMovesFormationCandidatesJobsAndAudit(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	_ = seedFormationTurn(t, store, "winner", "session", "Winner turn")
	turnID := seedFormationTurn(t, store, "loser", "session", "I use Go for Atlas")
	output := evaluatedFormationCandidate(t, "I use Go for Atlas", "I use Go for Atlas", "The user uses Go for Atlas.", memoryformation.CategoryProjects)
	publishedCandidate, _, err := store.ProposeCandidate(context.Background(), "loser", CandidateProposal{Output: output, IdempotencyKey: "published", Source: FormationSource{RequestID: "req-published", SessionID: "session", SessionGeneration: 1, TurnID: turnID}})
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.PublishCandidate(context.Background(), "loser", publishedCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposedOutput := evaluatedClaimCandidate(t, "Concise replies seem preferable.", "The user prefers concise replies.", memoryformation.CategoryCommunicationPreferences, memoryformation.ProvenanceModelInference, memoryformation.SensitivityLow, 0.2, "communication.reply_style", "concise")
	proposed, _, err := store.ProposeCandidate(context.Background(), "loser", CandidateProposal{Output: proposedOutput, IdempotencyKey: "proposed", Source: FormationSource{RequestID: "req-proposed", SessionID: "session", SessionGeneration: 1, TurnID: turnID}})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := store.EnqueueFormationJob(context.Background(), FormationSource{RequestID: "req-job", SessionID: "session", SessionGeneration: 1, TurnID: turnID, ExtractorVersion: FormationExtractorVersion}, "loser")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MergeUsers("winner", "loser"); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("winner", "", "", 10)
	if err != nil || len(memories) != 1 || memories[0].ID != published.ID {
		t.Fatalf("merged memories=%+v err=%v", memories, err)
	}
	mergedProposed, err := store.LoadCandidate(context.Background(), "winner", proposed.ID)
	if err != nil || mergedProposed.State != "proposed" || mergedProposed.SourceTurnID != turnID || mergedProposed.SourceGeneration != 2 {
		t.Fatalf("merged proposed=%+v err=%v", mergedProposed, err)
	}
	var evidenceGeneration int
	if err := store.sql.QueryRow(`SELECT source_session_generation FROM memory_evidence WHERE candidate_id = ?`, proposed.ID).Scan(&evidenceGeneration); err != nil || evidenceGeneration != mergedProposed.SourceGeneration {
		t.Fatalf("merged evidence generation=%d candidate=%d err=%v", evidenceGeneration, mergedProposed.SourceGeneration, err)
	}
	if _, err := store.FormationJobState(context.Background(), "winner", jobID); err != nil {
		t.Fatalf("merged job: %v", err)
	}
	var loserRows, auditRows int
	if err := store.sql.QueryRow(`SELECT (SELECT count(*) FROM memory_candidates WHERE canonical_user_id = 'loser') + (SELECT count(*) FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = 'loser')`).Scan(&loserRows); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT count(*) FROM memory_formation_audit WHERE canonical_user_id = 'winner'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if loserRows != 0 || auditRows == 0 {
		t.Fatalf("loser rows=%d winner audit rows=%d", loserRows, auditRows)
	}
}

func TestMergeConsolidatesClaimEvidenceAndConfidence(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "winner", "loser")
	ctx := context.Background()
	winnerOutput := evaluatedClaimCandidate(t, "Which pacman file manager is best?", "The user may use a pacman-based Linux environment.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceModelInference, memoryformation.SensitivityLow, 0.5, "environment.linux_distribution", "arch_family")
	winnerCandidate, _, err := store.ProposeCandidate(ctx, "winner", CandidateProposal{Output: winnerOutput, IdempotencyKey: "winner-arch"})
	if err != nil {
		t.Fatal(err)
	}
	winnerMemory, err := store.PublishCandidate(ctx, "winner", winnerCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	loserOutput := evaluatedClaimCandidate(t, "I use Arch Linux.", "The user uses Arch Linux.", memoryformation.CategoryEnvironment, memoryformation.ProvenanceUserStatement, memoryformation.SensitivityLow, 0.9, "environment.linux_distribution", "arch")
	loserCandidate, _, err := store.ProposeCandidate(ctx, "loser", CandidateProposal{Output: loserOutput, IdempotencyKey: "loser-arch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishCandidate(ctx, "loser", loserCandidate.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeUsers("winner", "loser"); err != nil {
		t.Fatal(err)
	}
	memories, err := store.ListMemories("winner", "", "", 10)
	if err != nil || len(memories) != 1 {
		t.Fatalf("merged memories=%+v err=%v", memories, err)
	}
	merged := memories[0]
	if merged.ID != winnerMemory.ID || merged.SourceAuthority != string(memoryformation.AuthorityUserDirect) || merged.EvidenceCount != 2 {
		t.Fatalf("merged confidence memory=%+v", merged)
	}
	if diff := merged.Confidence - 0.95; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("merged confidence=%v want=0.95", merged.Confidence)
	}
	var evidenceRows int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM memory_evidence WHERE canonical_user_id = 'winner' AND memory_id = ?`, merged.ID).Scan(&evidenceRows); err != nil || evidenceRows != 2 {
		t.Fatalf("merged evidence rows=%d err=%v", evidenceRows, err)
	}
}

func evaluatedFormationCandidate(t *testing.T, source, evidence, statement string, category memoryformation.Category) memoryformation.CandidateOutput {
	t.Helper()
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{SourceUserText: source, Statement: statement, Evidence: evidence, Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect, Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeAutomaticExtraction, Scope: memoryformation.ScopeLongTerm, Category: category, Context: memoryformation.ContextDirectAssertion, Confidence: 0.9, Importance: 4})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func evaluatedClaimCandidate(t *testing.T, source, statement string, category memoryformation.Category, provenance memoryformation.Provenance, sensitivity memoryformation.Sensitivity, confidence float64, claimSlot, claimValue string) memoryformation.CandidateOutput {
	t.Helper()
	claimedAuthority := memoryformation.AuthorityModel
	if provenance == memoryformation.ProvenanceUserStatement {
		claimedAuthority = memoryformation.AuthorityUserDirect
	}
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{SourceUserText: source, Statement: statement, Evidence: source, Provenance: provenance, ClaimedAuthority: claimedAuthority, Sensitivity: sensitivity, Mode: memoryformation.ModeAutomaticExtraction, Scope: memoryformation.ScopeLongTerm, Category: category, Context: memoryformation.ContextDirectAssertion, Confidence: confidence, Importance: 4, ClaimSlot: claimSlot, ClaimValue: claimValue})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func seedFormationTurn(t *testing.T, store *Store, userID, sessionID, userText string) int64 {
	t.Helper()
	now := formatTime(time.Now().UTC())
	result, err := store.sql.Exec(`INSERT INTO session_turns (session_id, canonical_user_id, session_generation, user_text, assistant_text, created_at) VALUES (?, ?, 1, ?, 'answer', ?)`, sessionID, userID, userText, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
