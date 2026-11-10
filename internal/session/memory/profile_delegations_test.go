package memory

import (
	"context"
	"testing"
	"time"
)

func insertDelegationFixture(t *testing.T, s *ProfileStore, id string) {
	t.Helper()
	err := s.InsertDelegation(context.Background(), DelegationRow{
		ID: id, OriginSession: "discord:dm:1", OriginSessionID: "discord:dm:1",
		TaskJSON: `{"goal":"synthetic"}`,
	})
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func TestDelegationClaimIsSingleWinner(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	insertDelegationFixture(t, s, "deleg-1")
	claimed, err := s.ClaimDelegation(ctx, "deleg-1", 4242, 1700000000)
	if err != nil || !claimed {
		t.Fatalf("claim = %v %v", claimed, err)
	}
	claimed, err = s.ClaimDelegation(ctx, "deleg-1", 9999, 1700000001)
	if err != nil || claimed {
		t.Fatalf("second claim = %v %v", claimed, err)
	}
	// Finishing under another owner leaves the row untouched.
	if err := s.FinishDelegation(ctx, "deleg-1", 9999, DelegationCompleted, `{"status":"completed"}`); err != nil {
		t.Fatal(err)
	}
	due, err := s.ListDueDeliveries(ctx, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("unfinished row listed due: %+v %v", due, err)
	}
	if err := s.FinishDelegation(ctx, "deleg-1", 4242, DelegationCompleted, `{"status":"completed"}`); err != nil {
		t.Fatal(err)
	}
	due, err = s.ListDueDeliveries(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ID != "deleg-1" {
		t.Fatalf("completed row not due: %+v %v", due, err)
	}
	if due[0].TaskJSON != `{"goal":"synthetic"}` || due[0].State != DelegationCompleted {
		t.Fatalf("row = %+v", due[0])
	}
	for _, state := range []string{"bogus", ""} {
		if err := s.FinishDelegation(ctx, "deleg-1", 4242, state, `{}`); err == nil {
			t.Fatalf("accepted state %q", state)
		}
	}
	if err := s.MarkDelivered(ctx, "deleg-1", float64(time.Now().UnixNano())/1e9); err != nil {
		t.Fatal(err)
	}
	due, err = s.ListDueDeliveries(ctx, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("delivered row still due: %+v %v", due, err)
	}
}

func TestDelegationDeliveryClaimAndRetry(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	now := float64(time.Now().UnixNano()) / 1e9
	insertDelegationFixture(t, s, "deleg-2")
	claimed, err := s.ClaimDelegation(ctx, "deleg-2", 4242, now)
	if err != nil || !claimed {
		t.Fatalf("claim = %v %v", claimed, err)
	}
	if err := s.FinishDelegation(ctx, "deleg-2", 4242, DelegationCompleted, `{"status":"completed"}`); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimDelivery(ctx, "deleg-2", "tok-1", now)
	if err != nil || !claimed {
		t.Fatalf("claim = %v %v", claimed, err)
	}
	if err := s.NoteDeliveryAttempt(ctx, "deleg-2", 1, float64(time.Now().UnixNano())/1e9); err != nil {
		t.Fatal(err)
	}
	due, err := s.ListDueDeliveries(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("retryable row missing: %+v %v", due, err)
	}
	if due[0].DeliveryAttempts != 1 {
		t.Fatalf("attempts = %+v", due[0])
	}
	if err := s.FailDelivery(ctx, "deleg-2", 9, float64(time.Now().UnixNano())/1e9); err != nil {
		t.Fatal(err)
	}
	due, err = s.ListDueDeliveries(ctx, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("failed delivery still due: %+v %v", due, err)
	}
}

func TestDelegationReapAndPrune(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	insertDelegationFixture(t, s, "live-1")
	insertDelegationFixture(t, s, "dead-1")
	insertDelegationFixture(t, s, "never-1")
	if _, err := s.ClaimDelegation(ctx, "live-1", 4242, 1700000000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDelegation(ctx, "dead-1", 9999, 1700000000); err != nil {
		t.Fatal(err)
	}
	alive := func(pid int64) bool { return pid == 4242 }
	reaped, err := s.ReapOrphanedDelegations(ctx, alive, float64(time.Now().UnixNano())/1e9)
	if err != nil || reaped != 2 {
		t.Fatalf("reaped = %d %v", reaped, err)
	}
	// Old terminal rows prune; live rows never do.
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE async_delegations SET delivery_state=?,updated_at=? WHERE delegation_id IN ('dead-1','never-1')`, DelegationDeliveryDelivered, old); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.PruneDelegations(ctx, time.Now().Add(-7*24*time.Hour).Unix(), 1000)
	if err != nil || deleted != 2 {
		t.Fatalf("pruned = %d %v", deleted, err)
	}
	due, err := s.ListDueDeliveries(ctx, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("unexpected due rows: %+v %v", due, err)
	}
	var live string
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT state FROM async_delegations WHERE delegation_id='live-1'`).Scan(&live); err != nil || live != DelegationRunning {
		t.Fatalf("live row touched: %q %v", live, err)
	}
}

func TestDelegationRejectsBadRows(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	for _, row := range []DelegationRow{
		{},
		{ID: "x"},
		{ID: "x", OriginSession: "s"},
	} {
		if err := s.InsertDelegation(ctx, row); err == nil {
			t.Fatalf("accepted %+v", row)
		}
	}
	if _, err := s.ClaimDelegation(ctx, "ghost", 0, 0); err == nil {
		t.Fatal("accepted pid 0")
	}
}
