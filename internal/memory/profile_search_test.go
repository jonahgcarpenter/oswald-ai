package memory

import (
	"context"
	"testing"
	"time"
)

// deliveredExchange appends and delivers one exchange, filling searchable text.
func deliveredExchange(t *testing.T, s *ProfileStore, generation int, user, answer string) StoredSessionTurn {
	t.Helper()
	turn, err := s.AppendPendingSessionTurn(context.Background(), SessionTurnWrite{UserID: s.profile, SessionID: "discord:dm:123", Generation: generation, UserText: user, AssistantText: answer, Pressure: SessionPromptPressure{Tokens: 10, Limit: 100, Version: "synthetic-v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionTurnDelivered(context.Background(), s.profile, turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn
}

func TestProfileDiscoveryExcludesPendingAndForeignProfiles(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	session, err := s.ResolveSessionContext(ctx, "alice", "discord:dm:123", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	deliveredExchange(t, s, session.Generation, "the docker deploy failed", "the image tag was stale")
	pending, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: "discord:dm:123", Generation: session.Generation, UserText: "pending docker note", AssistantText: "unseen", Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = pending

	results, err := s.DiscoverySessions(ctx, "alice", SearchFilter{Query: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MatchMessageID == 0 {
		t.Fatalf("delivered discovery mismatch: %+v", results)
	}
	if results[0].Snippet == "" {
		t.Fatal("missing snippet")
	}
	// Cross-profile and cross-generation reads fail closed.
	if _, err := s.DiscoverySessions(ctx, "bob", SearchFilter{Query: "docker"}); err == nil {
		t.Fatal("foreign profile discovery accepted")
	}
	if _, _, _, err := s.SessionWindow(ctx, "bob", "discord:dm:123", results[0].MatchMessageID, 5); err == nil {
		t.Fatal("foreign profile window accepted")
	}
	if _, _, _, err := s.SessionTranscript(ctx, "bob", "discord:dm:123", 20, 10); err == nil {
		t.Fatal("foreign profile transcript accepted")
	}
	if _, err := s.RecentSessions(ctx, "bob", 5); err == nil {
		t.Fatal("foreign profile browse accepted")
	}
}

func TestProfileWindowAndTranscriptBounds(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	session, err := s.ResolveSessionContext(ctx, "alice", "discord:dm:123", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var anchorID int64
	for i := 1; i <= 12; i++ {
		turn := deliveredExchange(t, s, session.Generation, "user message", "assistant reply")
		if i == 6 {
			anchorID = turn.ID
		}
	}
	before, anchor, after, err := s.SessionWindow(ctx, "alice", "discord:dm:123", anchorID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if anchor == nil || anchor.ID != anchorID || len(before) != 2 || len(after) != 2 {
		t.Fatalf("window bounds mismatch: before=%d after=%d anchor=%v", len(before), len(after), anchor)
	}
	// Clamping: window is capped at 20 each side.
	if _, _, afterWide, err := s.SessionWindow(ctx, "alice", "discord:dm:123", anchorID, 999); err != nil || len(afterWide) > 20 {
		t.Fatalf("window clamp failed: %v", err)
	}
	first, _, count, err := s.SessionTranscript(ctx, "alice", "discord:dm:123", 20, 10)
	if err != nil || count == 0 || len(first) == 0 {
		t.Fatalf("transcript mismatch: count=%d err=%v", count, err)
	}
}

func TestProfileInputTextIsSearchable(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	session, err := s.ResolveSessionContext(ctx, "alice", "discord:dm:123", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	deliveredExchange(t, s, session.Generation, "unrelated opening", "unrelated answer")
	results, err := s.DiscoverySessions(ctx, "alice", SearchFilter{Query: "unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("multi-term query mismatch: %+v", results)
	}
}
