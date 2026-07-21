package usermemory

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memoryformation"
)

func TestPrivacySessionDeleteFindsBindinglessArtifactGeneration(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "private", "answer", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`DELETE FROM sessions WHERE canonical_user_id = 'user' AND session_id = 'session'`); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteSessionPrivacy(ctx, "user", hashText("actor"), "session", "bindingless-delete", time.Now().UTC())
	if err != nil || deleted != profile.Generation {
		t.Fatalf("generation=%d want=%d err=%v", deleted, profile.Generation, err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM session_turns WHERE id = ?`, 0, turn.ID)
}

func TestDeleteAllMemoriesCancelsLeasedFormationJob(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	profile, err := store.ResolveSessionProfile(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendSessionTurnForGenerationResult(ctx, "session", "user", profile.Generation, "I use Go", "noted", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFormationEligible(ctx, "user", turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueFormationJob(ctx, FormationSource{RequestID: "source", SessionID: "session", SessionGeneration: profile.Generation, TurnID: turn.ID}, "user"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimFormationJob(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.CreatePrivacyChallenge(ctx, "user", hashText("actor"), "delete-all", "delete_all_memories", hashText("all"), hashText("code"), now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmPrivacyChallenge(ctx, "user", hashText("actor"), hashText("code"), "confirm", now); err != nil {
		t.Fatal(err)
	}
	output := evaluatedFormationCandidate(t, "I use Go", "I use Go", "The user uses Go.", memoryformation.CategoryProjects)
	_, _, err = store.ProposeCandidate(ctx, "user", CandidateProposal{Output: output, Source: FormationSource{RequestID: job.RequestID, SessionID: job.SessionID, SessionGeneration: job.SessionGeneration, TurnID: job.TurnID}, IdempotencyKey: "stale-job", FormationJob: &job})
	if err == nil || !strings.Contains(err.Error(), "stale or cancelled") {
		t.Fatalf("stale proposal err=%v", err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM durable_jobs WHERE job_kind = 'memory_formation' AND canonical_user_id = 'user'`, 0)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM memory_candidates WHERE canonical_user_id = 'user' AND statement != ''`, 0)
}

func TestEraseUserRetainsAndTerminatesAllPrivacyOperations(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	now := time.Now().UTC()
	hash := hashText("actor")
	for i, status := range []string{"pending", "pending", "running", "completed"} {
		id := "operation-" + strconv.Itoa(i)
		challenge, expiry := "", any(nil)
		if status == "pending" {
			challenge, expiry = hashText(id), formatTime(now.Add(time.Hour))
		}
		if _, err := store.sql.Exec(`INSERT INTO privacy_operations(operation_id,idempotency_key,actor_hash,target_user_id,target_hash,operation_type,target_digest,challenge_hash,challenge_expires_at,status,created_at,updated_at) VALUES (?,?,?,?,?,'delete_user',?,?,?,?,?,?)`, id, id, hash, "user", hashText("user"), hashText(id+"-target"), challenge, expiry, status, formatTime(now), formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := store.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseUserTx(ctx, tx, "user", now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM privacy_operations`, 4)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM privacy_operations WHERE target_user_id IS NULL AND challenge_hash = '' AND challenge_expires_at IS NULL AND status IN ('completed','failed','expired')`, 4)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM privacy_operations WHERE status = 'expired'`, 2)
	assertPrivacyCount(t, store.sql, `SELECT COUNT(*) FROM privacy_operations WHERE status = 'failed'`, 1)
}

func TestPrivacyOperationRequestIDCollisionFails(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	now := time.Now().UTC()
	if err := store.RecordPrivacyExport(ctx, "user", hashText("actor"), "same-request", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSessionPrivacy(ctx, "user", hashText("actor"), "session", "same-request", now); err == nil || !strings.Contains(err.Error(), "payload mismatch") {
		t.Fatalf("collision err=%v", err)
	}
}

func TestMaintenanceSweepExpiresPendingPrivacyChallenge(t *testing.T) {
	ctx := context.Background()
	store := NewStore(t.TempDir()+"/oswald.db", config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	now := time.Now().UTC()
	hash := hashText("actor")
	if _, err := store.sql.Exec(`INSERT INTO privacy_operations(operation_id,idempotency_key,actor_hash,target_user_id,target_hash,operation_type,target_digest,challenge_hash,challenge_expires_at,status,created_at,updated_at) VALUES ('pending','pending',?,'user',?,'delete_user',?,?,?,'pending',?,?)`, hash, hashText("user"), hashText("target"), hashText("code"), formatTime(now.Add(-time.Second)), formatTime(now.Add(-time.Hour)), formatTime(now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	counts, err := store.MaintenanceSweep(ctx, now, maintenanceTestPolicy())
	if err != nil || counts.PrivacyChallengesExpired != 1 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
	var status, challenge string
	var target, expires sql.NullString
	if err := store.sql.QueryRow(`SELECT status, target_user_id, challenge_hash, challenge_expires_at FROM privacy_operations WHERE operation_id = 'pending'`).Scan(&status, &target, &challenge, &expires); err != nil {
		t.Fatal(err)
	}
	if status != "expired" || target.Valid || challenge != "" || expires.Valid {
		t.Fatalf("status=%q target=%v challenge=%q expires=%v", status, target, challenge, expires)
	}
}
