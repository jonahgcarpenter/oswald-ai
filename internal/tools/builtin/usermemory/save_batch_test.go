package usermemory

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

func TestSubmitMemorySaveBatchPreservesExplicitRememberModeFromUserWording(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { store.Close() })
	seedAccountUsers(t, store, "user-1")

	outcomes := store.SubmitMemorySaveBatch(context.Background(), "user-1", "Remember that I prefer concise replies.", FormationSource{RequestID: "explicit"}, MemorySaveBatch{Memories: []MemorySaveItem{{
		Statement: "The user prefers concise replies.", Evidence: "I prefer concise replies.",
		Scope: "long_term", Category: "communication_preferences", Context: "direct_assertion",
		Provenance: "user_statement", Sensitivity: "low", Confidence: 0.95, Importance: 4,
		ClaimSlot: "communication.reply_style", ClaimValue: "concise",
	}}}, nil)
	if len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Fatalf("outcomes=%+v", outcomes)
	}
	candidate, err := store.LoadCandidate(context.Background(), "user-1", outcomes[0].CandidateID)
	if err != nil || candidate.FormationMode != string(memoryformation.ModeExplicitRemember) {
		t.Fatalf("candidate=%+v err=%v", candidate, err)
	}
}
