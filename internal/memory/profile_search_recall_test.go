package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func searchTurn(t *testing.T, store *ProfileStore, key, user, answer string, history ToolHistory, delivered bool) (StoredSessionTurn, string) {
	t.Helper()
	ctx := context.Background()
	session, err := store.ResolveSessionContext(ctx, store.profile, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: store.profile, SessionID: key, Generation: session.Generation, UserText: user, AssistantText: answer, History: history, Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if delivered {
		if err := store.MarkSessionTurnDelivered(ctx, store.profile, turn.ID); err != nil {
			t.Fatal(err)
		}
	}
	id, err := store.ActiveSessionID(ctx, store.profile, key, session.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return turn, id
}

func recallTrace(name, text string, searchable bool) ToolHistory {
	return ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{Calls: []ToolHistoryCall{{Name: name, Arguments: map[string]interface{}{"question": "retained question"}, Result: text, Status: "succeeded", HistoryMode: "full", SearchResult: searchable}}}}}
}

func TestDiscoveryRolesTitlesAndNativeToolSearchPolicy(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	turn, id := searchTurn(t, s, "discord:dm:first", "userword", "assistantword", recallTrace("web_search", "connection refused toolword", true), true)
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE sessions SET title='Deployment checklist' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	searchTurn(t, s, "discord:dm:second", "other", "reply", recallTrace("vision_analyze", "privateimageword", false), true)
	searchTurn(t, s, "discord:dm:pending", "pendingword", "pendinganswer", recallTrace("web_search", "pendingtoolword", true), false)
	for _, test := range []struct {
		query string
		roles map[string]bool
		want  int
		role  string
	}{
		{"userword", nil, 1, "user"},
		{"userword", map[string]bool{"assistant": true}, 0, ""},
		{"assistantword", map[string]bool{"assistant": true}, 1, "assistant"},
		{"toolword", nil, 0, ""},
		{"\"connection refused\"", map[string]bool{"tool": true}, 1, "tool"},
		{"toolword", map[string]bool{"user": true, "assistant": true, "tool": true}, 1, "tool"},
		{"deploy*", nil, 1, "title"},
		{"Deployment", map[string]bool{"tool": true}, 0, ""},
		{"privateimageword", map[string]bool{"tool": true}, 0, ""},
		{"pendingword OR pendingtoolword", map[string]bool{"user": true, "tool": true}, 0, ""},
		{"userword NOT assistantword", nil, 1, "user"},
	} {
		t.Run(test.query+fmt.Sprint(test.roles), func(t *testing.T) {
			results, err := s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: test.query, Roles: test.roles})
			if err != nil || len(results) != test.want {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			if test.want > 0 && (results[0].MatchedRole != test.role || results[0].SessionID != id) {
				t.Fatalf("wrong match: %+v", results)
			}
			if test.role == "tool" && results[0].MatchMessageID != turn.ID {
				t.Fatal("native tool match did not use its stable assistant message anchor")
			}
		})
	}
	first, _, _, err := s.SessionTranscript(ctx, s.profile, id, 20, 10)
	if err != nil || len(first) != 4 || len(first[3].History.Batches) != 1 || first[3].History.Batches[0].Calls[0].Result != "connection refused toolword" {
		t.Fatalf("native tool trace was not hydrated: %+v err=%v", first, err)
	}
	if first[0].Role != "user" || first[1].Role != "assistant" || first[1].ToolCalls == "" || first[2].Role != "tool" || first[2].ToolCallID == "" || first[3].Role != "assistant" {
		t.Fatalf("tool transcript rows missing linkage: %+v", first)
	}
}

func TestDiscoveryFiltersAndLineageDoNotStarveResults(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	_, live := searchTurn(t, s, "discord:dm:live", "docker", "reply", EmptyToolHistory(), true)
	for i := 0; i < 20; i++ {
		searchTurn(t, s, "discord:dm:live", "docker", "reply", EmptyToolHistory(), true)
	}
	_, old := searchTurn(t, s, "discord:dm:old", "docker", "reply", EmptyToolHistory(), true)
	_, wanted := searchTurn(t, s, "discord:dm:wanted", "docker", "reply", EmptyToolHistory(), true)
	started := time.Now().UTC().Add(-72 * time.Hour)
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE sessions SET started_at=? WHERE id=?`, float64(started.Unix()), old); err != nil {
		t.Fatal(err)
	}
	bound := time.Now().UTC().Add(-24 * time.Hour)
	results, err := s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "docker", Limit: 1, LiveSessionID: live, After: &bound})
	if err != nil || len(results) != 1 || results[0].SessionID != wanted {
		t.Fatalf("excluded hits hid a valid session: %+v err=%v", results, err)
	}
	results, err = s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "docker", Limit: 1, Before: &bound})
	if err != nil || len(results) != 1 || results[0].SessionID != old {
		t.Fatalf("bounds used message time instead of session start: %+v err=%v", results, err)
	}
	_, child := searchTurn(t, s, "discord:dm:child", "childkeyword docker", "reply", EmptyToolHistory(), true)
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE sessions SET parent_session_id=? WHERE id=?`, wanted, child); err != nil {
		t.Fatal(err)
	}
	results, err = s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "childkeyword"})
	if err != nil || len(results) != 1 || results[0].SessionID != child {
		t.Fatalf("child match was redirected to parent transcript: %+v err=%v", results, err)
	}
	results, err = s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "docker", Exclude: []string{live, wanted, old}})
	if err != nil || len(results) != 0 {
		t.Fatalf("lineage exclusion missed child: %+v err=%v", results, err)
	}
	for _, sort := range []string{"newest", "oldest"} {
		results, err = s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "docker", Sort: sort, Exclude: []string{live, child}})
		if err != nil || len(results) != 1 || results[0].SessionID != old {
			t.Fatalf("temporal ranking failed: %+v err=%v", results, err)
		}
	}
}

func TestSearchWindowsStayInsideConcreteSessionAndReadWholeShortSessions(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	_, id := searchTurn(t, s, "discord:dm:first", "first user", "first answer", EmptyToolHistory(), true)
	foreign, otherID := searchTurn(t, s, "discord:dm:other", "other user", "other answer", EmptyToolHistory(), true)
	var anchor int64
	for i := 0; i < 11; i++ {
		turn, _ := searchTurn(t, s, "discord:dm:first", "first user", "first answer", EmptyToolHistory(), true)
		anchor = turn.ID
	}
	first, last, count, err := s.SessionTranscript(ctx, s.profile, id, 20, 10)
	if err != nil || count != 24 || len(first) != 24 || len(last) != 0 || first[23].ID != anchor {
		t.Fatalf("short transcript dropped its tail: first=%d last=%d count=%d err=%v", len(first), len(last), count, err)
	}
	before, _, after, err := s.SessionWindow(ctx, s.profile, id, first[1].ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range append(before, after...) {
		if strings.Contains(message.Content, "other") {
			t.Fatal("scroll included unrelated session messages")
		}
	}
	if _, _, _, err := s.SessionWindow(ctx, s.profile, id, foreign.ID, 5); err == nil {
		t.Fatal("foreign-session anchor was accepted")
	}
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE sessions SET parent_session_id=? WHERE id=?`, id, otherID); err != nil {
		t.Fatal(err)
	}
	child, _, count, err := s.SessionTranscript(ctx, s.profile, otherID, 20, 10)
	if err != nil || count != 2 || child[1].ID != foreign.ID {
		t.Fatalf("read redirected child to root: %+v count=%d err=%v", child, count, err)
	}
	for i := 0; i < 5; i++ {
		searchTurn(t, s, "discord:dm:first", "first user", "first answer", EmptyToolHistory(), true)
	}
	first, last, count, err = s.SessionTranscript(ctx, s.profile, id, 20, 10)
	if err != nil || count != 34 || len(first) != 20 || len(last) != 10 {
		t.Fatalf("long transcript bounds: first=%d last=%d count=%d err=%v", len(first), len(last), count, err)
	}
}

func TestBrowseOnlyDeliveredDistinctLineagesAndResetHistoryRemainsSearchable(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	_, id := searchTurn(t, s, "discord:dm:past", "resetkeyword", "answer", EmptyToolHistory(), true)
	if err := s.NewSessionContext(ctx, s.profile, "discord:dm:past"); err != nil {
		t.Fatal(err)
	}
	_, live := searchTurn(t, s, "discord:dm:past", "new session", "answer", EmptyToolHistory(), true)
	searchTurn(t, s, "discord:dm:pending", "privatepending", "privatepending", EmptyToolHistory(), false)
	results, err := s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "resetkeyword", LiveSessionID: live})
	if err != nil || len(results) != 1 || results[0].SessionID != id {
		t.Fatalf("reset history was not searchable: %+v err=%v", results, err)
	}
	for i := 0; i < 5; i++ {
		_, child := searchTurn(t, s, fmt.Sprintf("discord:dm:child%d", i), "child", "answer", EmptyToolHistory(), true)
		if _, err := s.db.SQL().ExecContext(ctx, `UPDATE sessions SET parent_session_id=? WHERE id=?`, live, child); err != nil {
			t.Fatal(err)
		}
	}
	records, err := s.RecentSessionsExcluding(ctx, s.profile, live, 1)
	if err != nil || len(records) != 1 || records[0].SessionID != id || records[0].MessageCount != 2 {
		t.Fatalf("browse was starved by pending/live lineage: %+v err=%v", records, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.DiscoverySessions(canceled, s.profile, SearchFilter{Query: "resetkeyword"}); err == nil {
		t.Fatal("canceled discovery succeeded")
	}
	if _, err := s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "\"unterminated"}); err == nil {
		t.Fatal("invalid FTS syntax succeeded")
	}
}

func TestRecallDoesNotMutateSchemaOrHideDeliveredCompactedMessages(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	_, id := searchTurn(t, s, "discord:dm:past", "compressedkeyword", "answer", EmptyToolHistory(), true)
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE messages SET compacted=1 WHERE session_id=?`, id); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT group_concat(sql) FROM (SELECT sql FROM sqlite_master ORDER BY type,name)`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	results, err := s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "compressedkeyword"})
	if err != nil || len(results) != 1 {
		t.Fatalf("delivered compacted messages became unsearchable: %+v err=%v", results, err)
	}
	var after string
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT group_concat(sql) FROM (SELECT sql FROM sqlite_master ORDER BY type,name)`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("recall changed operator schema objects")
	}
	if _, _, err := s.SessionWindowRemainder(ctx, "bob", id, results[0].MatchMessageID, results[0].MatchMessageID); err == nil {
		t.Fatal("foreign profile counted message windows")
	}
}

func TestDiscoveryTemporalPreferenceBiasesEqualRelevance(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	_, older := searchTurn(t, s, "discord:dm:older", "rankneedle", "answer", EmptyToolHistory(), true)
	_, newer := searchTurn(t, s, "discord:dm:newer", "rankneedle", "answer", EmptyToolHistory(), true)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE sessions SET started_at=? WHERE id=?`, float64(now.Add(-72*time.Hour).Unix()), older); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL().ExecContext(ctx, `UPDATE sessions SET started_at=? WHERE id=?`, float64(now.Add(-time.Hour).Unix()), newer); err != nil {
		t.Fatal(err)
	}
	for sort, want := range map[string]string{"newest": newer, "oldest": older} {
		results, err := s.DiscoverySessions(ctx, s.profile, SearchFilter{Query: "rankneedle", Sort: sort, Limit: 2})
		if err != nil || len(results) != 2 || results[0].SessionID != want {
			t.Fatalf("%s ranking: %+v err=%v", sort, results, err)
		}
	}
}
