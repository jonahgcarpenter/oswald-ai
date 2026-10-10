package memory

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestRecordModelUsageAccumulatesCalls(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	chat := ModelUsageRecord{SessionID: key, UserID: "alice", Generation: session.Generation, Model: "test-model", BillingProvider: "custom", Task: "", ApiCalls: 1, PromptTokens: 100, CompletionTokens: 10}
	if err := s.RecordModelUsage(ctx, chat); err != nil {
		t.Fatal(err)
	}
	chat.PromptTokens, chat.CompletionTokens = 50, 5
	if err := s.RecordModelUsage(ctx, chat); err != nil {
		t.Fatal(err)
	}
	compression := chat
	compression.Task = "compression"
	compression.PromptTokens, compression.CompletionTokens = 200, 20
	if err := s.RecordModelUsage(ctx, compression); err != nil {
		t.Fatal(err)
	}
	usage, err := s.SessionModelUsage(ctx, "alice", key, session.Generation)
	if err != nil || len(usage) != 2 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	var main, task *ModelUsageRow
	for i := range usage {
		if usage[i].Task == "" {
			main = &usage[i]
		} else {
			task = &usage[i]
		}
	}
	if main == nil || task == nil {
		t.Fatalf("task rows not separated: %+v", usage)
	}
	if main.ApiCalls != 2 || main.PromptTokens != 150 || main.CompletionTokens != 15 {
		t.Fatalf("chat counters did not accumulate: %+v", main)
	}
	if main.FirstSeen <= 0 || main.LastSeen < main.FirstSeen {
		t.Fatalf("usage window malformed: %+v", main)
	}
	if task.ApiCalls != 1 || task.PromptTokens != 200 || task.Model != "test-model" {
		t.Fatalf("compression row malformed: %+v", task)
	}
}

func TestRecordModelUsageRejectsInvalidScope(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	valid := ModelUsageRecord{SessionID: key, UserID: "alice", Generation: session.Generation, Model: "m", ApiCalls: 1}
	stale := valid
	stale.Generation = session.Generation + 100
	if err := s.RecordModelUsage(ctx, stale); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale generation recorded: %v", err)
	}
	foreign := valid
	foreign.UserID = "bob"
	if err := s.RecordModelUsage(ctx, foreign); err == nil {
		t.Fatal("cross-profile usage accepted")
	}
	nameless := valid
	nameless.Model = ""
	if err := s.RecordModelUsage(ctx, nameless); err == nil {
		t.Fatal("model-less usage accepted")
	}
	negative := valid
	negative.PromptTokens = -1
	if err := s.RecordModelUsage(ctx, negative); err == nil {
		t.Fatal("negative usage accepted")
	}
	var count int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM session_model_usage`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected usage persisted: count=%d err=%v", count, err)
	}
}
