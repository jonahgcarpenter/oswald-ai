package usermemory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

func TestForegroundMemoryRoundTripOnSessionTurn(t *testing.T) {
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(context.Background(), "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	output, err := memoryformation.Evaluate(memoryformation.CandidateInput{
		SourceUserText: "I prefer dark mode.", Statement: "The user prefers dark mode.", Evidence: "I prefer dark mode.",
		Provenance: memoryformation.ProvenanceUserStatement, ClaimedAuthority: memoryformation.AuthorityUserDirect,
		Sensitivity: memoryformation.SensitivityLow, Mode: memoryformation.ModeAgentSave, Scope: memoryformation.ScopeLongTerm,
		Category: memoryformation.CategoryDurablePreferences, Context: memoryformation.ContextDirectAssertion,
		Confidence: 1, Importance: 3, ClaimSlot: "durable_preferences.fact", ClaimValue: "dark mode",
	})
	if err != nil || output.Approval != memoryformation.ApprovalApproved {
		t.Fatalf("candidate=%+v err=%v", output, err)
	}
	turn, err := store.AppendSessionTurnForGenerationResultWithPressureHistoryAndForegroundMemory(
		context.Background(), "session", "user", profile.Generation, "I prefer dark mode.", "Noted.", nil,
		EmptyToolHistory(), []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: output}}, time.Hour,
		SessionPromptPressure{Tokens: 10, Limit: 100, Version: "test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.SessionTurnForegroundMemory(context.Background(), "user", turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Candidates) != 1 || artifact.Candidates[0].Statement != output.Statement || artifact.Candidates[0].Provenance != memoryformation.ProvenanceUserStatement || artifact.Candidates[0].EvidenceType != "direct_statement" || artifact.Candidates[0].Confidence != 1 {
		t.Fatalf("artifact=%+v", artifact)
	}
	if err := store.MarkFormationEligible(context.Background(), "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	source := FormationSource{SessionID: turn.SessionID, SessionGeneration: turn.Generation, TurnID: turn.ID, Model: "model"}
	firstJobID, created, err := store.EnqueueAgentSaveFormationJob(context.Background(), source, "user")
	if err != nil || !created {
		t.Fatalf("first enqueue id=%d created=%v err=%v", firstJobID, created, err)
	}
	secondJobID, created, err := store.EnqueueAgentSaveFormationJob(context.Background(), source, "user")
	if err != nil || created || secondJobID != firstJobID {
		t.Fatalf("duplicate enqueue id=%d want=%d created=%v err=%v", secondJobID, firstJobID, created, err)
	}
	rechecked, err := artifact.Candidates[0].Evaluate("I prefer dark mode.")
	if err != nil || rechecked.Approval != memoryformation.ApprovalApproved {
		t.Fatalf("rechecked=%+v err=%v", rechecked, err)
	}
	var raw string
	if err := store.sql.QueryRow(`SELECT foreground_memory FROM session_turns WHERE id = ?`, turn.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"approval", "decision", "sensitivity", "source_authority"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("persisted policy output %q in %s", forbidden, raw)
		}
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET foreground_memory = '{"version":2,"candidates":[]}' WHERE id = ?`, turn.ID); err == nil {
		t.Fatal("mutable foreground memory artifact was accepted")
	}
}

func TestForegroundMemoryEncodingBoundsAndOwnership(t *testing.T) {
	approved := memoryformation.CandidateOutput{
		Statement: strings.Repeat("x", MaxForegroundMemoryBytes), Evidence: "I use x.", Category: memoryformation.CategoryNotes,
		ClaimSlot: "notes.fact", ClaimValue: "x", Provenance: memoryformation.ProvenanceUserStatement, Mode: memoryformation.ModeAgentSave, Approval: memoryformation.ApprovalApproved, Confidence: 1,
	}
	if _, err := EncodeForegroundMemory("user", []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: approved}}); err == nil {
		t.Fatal("oversized foreground memory was accepted")
	}
	approved.Statement = "The user uses x."
	if _, err := EncodeForegroundMemory("other", []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: approved}}); err == nil {
		t.Fatal("cross-tenant foreground memory was accepted")
	}
	if _, err := DecodeForegroundMemory(`{"version":2,"candidates":[],"unexpected":true}`); err == nil {
		t.Fatal("unknown artifact field was accepted")
	}
	if _, err := DecodeForegroundMemory(`{"version":2,"candidates":[{"statement":"The user likes tea.","evidence":"I like tea.","category":"identity","claim_slot":"preference.drink","claim_value":"tea","evidence_type":"direct_statement","provenance":"user_statement","confidence":1}]}`); err == nil {
		t.Fatal("policy-rejected artifact candidate was accepted")
	}
}

func TestForegroundMemoryPersistsExactModelAssessment(t *testing.T) {
	proposed := memoryformation.CandidateOutput{
		Statement: "The user might like pancakes.", Evidence: "I want pancakes.", Category: memoryformation.CategoryDurablePreferences,
		ClaimSlot: "preference.food", ClaimValue: "pancakes", Provenance: memoryformation.ProvenanceModelInference,
		Mode: memoryformation.ModeAgentSave, Approval: memoryformation.ApprovalProposed, Confidence: 0.2,
	}
	encoded, err := EncodeForegroundMemory("user", []requestctx.StagedMemoryCandidate{{CanonicalUserID: "user", Candidate: proposed, TargetMemoryID: 17, SupersedesStatement: "The user dislikes pancakes."}})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := DecodeForegroundMemory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	candidate := artifact.Candidates[0]
	if candidate.EvidenceType != "model_inference" || candidate.Provenance != memoryformation.ProvenanceModelInference || candidate.Confidence != 0.2 || candidate.TargetMemoryID != 17 || candidate.SupersedesStatement != "The user dislikes pancakes." {
		t.Fatalf("assessment was not retained: %+v", candidate)
	}
	rechecked, err := candidate.Evaluate("I want pancakes.")
	if err != nil || rechecked.Mode != memoryformation.ModeAgentSave || rechecked.Approval != memoryformation.ApprovalProposed || rechecked.Confidence != 0.2 || rechecked.SourceAuthority != memoryformation.AuthorityModel {
		t.Fatalf("assessment was not rerun exactly: %+v err=%v", rechecked, err)
	}
}
