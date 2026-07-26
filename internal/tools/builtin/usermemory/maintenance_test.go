package usermemory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestMaintenanceDirectlyDeletesStaleCandidateAndEventRows(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	t.Cleanup(func() { _ = store.Close() })
	seedAccountUsers(t, store, "user")
	old := formatTime(time.Now().UTC().Add(-48 * time.Hour))
	result, err := store.sql.Exec(`INSERT INTO memory_candidates(canonical_user_id,idempotency_key,state,publication_status,scope,category,statement,evidence,confidence,importance,provenance_type,extractor_version,formation_mode,sensitivity,created_at,updated_at,decision_reason,claim_slot,claim_value) VALUES ('user','stale','rejected','none','long_term','notes','The user has a stale candidate.','evidence',0.2,2,'user_statement','v1','automatic','low',?,?,'rejected','notes.stale','stale')`, old, old)
	if err != nil {
		t.Fatal(err)
	}
	candidateID, _ := result.LastInsertId()
	if _, err := store.sql.Exec(`INSERT INTO memory_events(canonical_user_id,event_kind,idempotency_key,event_type,candidate_id,created_at,metadata) VALUES ('user','formation_audit','stale-event','candidate.rejected',?,?,'details')`, candidateID, old); err != nil {
		t.Fatal(err)
	}
	policy := config.RetentionPolicy{RetiredIndexRetention: time.Hour, SessionInactivity: time.Hour, PendingDeliveryTimeout: time.Minute, SuccessfulJobRetention: time.Hour, DeadJobRetention: 24 * time.Hour, AccountChallengeGrace: time.Hour, MaintenanceInterval: time.Hour, DatabaseOptimizeInterval: time.Hour, BatchSize: 100}
	counts, err := store.MaintenanceSweep(context.Background(), time.Now().UTC(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if counts.CandidatesDeleted+counts.SessionCleanup.CandidatesErased != 1 || counts.EventsDeleted != 1 {
		t.Fatalf("maintenance counts=%+v", counts)
	}
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE id = ?`, 0, candidateID)
	assertStoreCount(t, store.sql, `SELECT COUNT(*) FROM memory_events WHERE candidate_id = ?`, 0, candidateID)
}
