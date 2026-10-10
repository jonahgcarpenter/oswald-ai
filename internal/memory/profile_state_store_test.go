package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func newProfileStateFixture(t *testing.T) (*ProfileStore, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewProfileStore(context.Background(), root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, root
}

func appendProfileExchange(t *testing.T, s *ProfileStore, generation int, text string) StoredSessionTurn {
	t.Helper()
	turn, err := s.AppendPendingSessionTurn(context.Background(), SessionTurnWrite{UserID: "alice", SessionID: "discord:dm:123", Generation: generation, UserText: text, AssistantText: "synthetic answer", Pressure: SessionPromptPressure{Tokens: 10, Limit: 100, Version: "synthetic-v1"}})
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func TestProfileExchangeDeliveryAndReopen(t *testing.T) {
	s, root := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	session, err := s.ResolveSessionContext(ctx, "alice", key, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !session.IsNewSession || session.Generation != 1 || session.StartedAt.IsZero() {
		t.Fatal("incorrect first generation")
	}
	first := appendProfileExchange(t, s, session.Generation, "first")
	second := appendProfileExchange(t, s, session.Generation, "second")
	if err := s.MarkSessionTurnDelivered(ctx, "alice", second.ID); err != nil {
		t.Fatal(err)
	}
	turns, err := s.PageDeliveredSessionTurnsAfter(ctx, "alice", key, 1, 0, 100)
	if err != nil || len(turns) != 1 || turns[0].ID != second.ID {
		t.Fatal("pending exchange entered context", err)
	}
	if err := s.MarkSessionTurnDeliveryFailed(ctx, "alice", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionTurnDelivered(ctx, "alice", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionTurnDeliveryFailed(ctx, "alice", first.ID); err != nil {
		t.Fatal("failure after success must be idempotent", err)
	}
	turns, err = s.PageDeliveredSessionTurnsAfter(ctx, "alice", key, 1, 0, 1)
	if err != nil || len(turns) != 1 || turns[0].ID != first.ID {
		t.Fatal("late delivery was not eligible", err)
	}
	turns, err = s.PageDeliveredSessionTurnsAfter(ctx, "alice", key, 1, first.ID, 1)
	if err != nil || len(turns) != 1 || turns[0].ID != second.ID {
		t.Fatal("paging failed", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewProfileStore(ctx, root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	session, err = reopened.ResolveSessionContext(ctx, "alice", key, 24*time.Hour)
	if err != nil || session.IsNewSession || session.Generation != 1 || session.StartedAt.IsZero() {
		t.Fatal("restart advanced generation", err)
	}
	turns, err = reopened.PageDeliveredSessionTurnsAfter(ctx, "alice", key, 1, 0, 100)
	if err != nil || len(turns) != 2 {
		t.Fatal("reopen lost delivered turns", err)
	}
	if _, err := reopened.PageDeliveredSessionTurnsAfter(ctx, "bob", key, 1, 0, 100); err == nil {
		t.Fatal("cross-profile query accepted")
	}
}

func TestProfileSnapshotNewSessionAndExpiryFences(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, bound, err := s.SessionFileMemory(ctx, "alice", key, session.Generation); err != nil || bound {
		t.Fatal("uncaptured files appeared bound", err)
	}
	u, m, err := s.BindSessionFileMemory(ctx, "alice", key, session.Generation, "", "")
	if err != nil || u != "" || m != "" {
		t.Fatal(err)
	}
	u, m, err = s.BindSessionFileMemory(ctx, "alice", key, session.Generation, "later user", "later memory")
	if err != nil || u != "" || m != "" {
		t.Fatal("empty first snapshot was overwritten", err)
	}
	turn := appendProfileExchange(t, s, 1, "before new session")
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	// The ended session is no longer the active target for its generation.
	if _, _, err := s.BindSessionFileMemory(ctx, "alice", key, 1, "stale", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("stale snapshot bound", err)
	}
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("delivery published into an ended session", err)
	}
	// The prior transcript is preserved and remains searchable.
	var preserved int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m JOIN sessions x ON x.id=m.session_id WHERE x.profile_name='alice' AND m.active=1`).Scan(&preserved); err != nil || preserved == 0 {
		t.Fatal("new session destroyed prior delivered history", err)
	}
	session, err = s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil || session.Generation != 2 {
		t.Fatal("new session failed to advance generation", err)
	}
	if _, _, bound, err := s.SessionFileMemory(ctx, "alice", key, 2); err != nil || bound {
		t.Fatal("new session retained the prior snapshot", err)
	}
	// The prior delivered transcript survives until its own TTL lapses.
	now = now.Add(2 * time.Hour)
	if _, err := s.PageDeliveredSessionTurnsAfter(ctx, "alice", key, 2, 0, 100); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("expired generation served from the live session", err)
	}
	session, err = s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil || session.Generation != 3 {
		t.Fatal("expiry failed to advance generation", err)
	}
}

func TestNewSessionKeepsPriorTranscriptSearchableUntilExpiry(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	deliveredExchange(t, s, 1, "the postgres migration plan", "use pg_upgrade")
	if err := s.NewSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	results, err := s.DiscoverySessions(ctx, "alice", SearchFilter{Query: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("prior transcript not searchable after new session: %+v", results)
	}
	// The old session is not the live conversation: exclusion hides it.
	live, err := s.ActiveSessionID(ctx, "alice", key, 2)
	if err != nil {
		t.Fatal(err)
	}
	if live != "" {
		t.Fatal("new session should not be open before the next turn")
	}
}

func TestProfileConcurrentFirstSnapshotIsOneWinningPair(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan [2]string, 10)
	failures := make(chan error, 10)
	for index := range 10 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			value := strings.Repeat("x", index)
			user, memory, err := s.BindSessionFileMemory(ctx, "alice", key, 1, value, value)
			results <- [2]string{user, memory}
			failures <- err
		}(index)
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var winner *[2]string
	for pair := range results {
		if pair[0] != pair[1] {
			t.Fatal("mixed snapshot pair")
		}
		if winner == nil {
			copy := pair
			winner = &copy
		} else if *winner != pair {
			t.Fatal("first-writer snapshot differed")
		}
	}
}

func TestProfileExchangeRollbackAndNewSessionPreservesFTS(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	// A deliberate synthetic failure after both INSERTs must roll back the full
	// exchange, including the supplied synchronous FTS triggers.
	if _, err := s.db.SQL().Exec(`CREATE TRIGGER synthetic_metadata_failure BEFORE INSERT ON state_meta WHEN new.key LIKE 'oswald:v1:turn:%' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: key, Generation: 1, UserText: "rollback elephant", AssistantText: "answer", Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}}); err == nil {
		t.Fatal("failed exchange committed")
	}
	var count int
	if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial messages retained", err)
	}
	if _, err := s.db.SQL().Exec(`DROP TRIGGER synthetic_metadata_failure`); err != nil {
		t.Fatal(err)
	}
	turn := appendProfileExchange(t, s, 1, "searchable elephant")
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	// /new is bookkeeping only: the transcript and its FTS text are preserved.
	if err := s.NewSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages_fts", "messages_fts_trigram"} {
		if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE ` + table + ` MATCH 'elephant'`).Scan(&count); err != nil || count != 1 {
			t.Fatal("new session dropped searchable FTS text", err)
		}
	}
	if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM messages WHERE content LIKE '%elephant%' AND active=1`).Scan(&count); err != nil || count != 1 {
		t.Fatal("new session dropped delivered messages", err)
	}
}

func TestAppendPersistsToolTranscriptRows(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	history := ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{
		AssistantContent: "checking",
		Calls: []ToolHistoryCall{
			{Name: "web_search", ProviderCallID: "call_provided_1", Arguments: map[string]interface{}{"query": "x"}, Result: "result one", Status: "succeeded", ExecutedAt: time.Now().UTC().Format(time.RFC3339Nano)},
			{Name: "memory", Arguments: map[string]interface{}{"op": "y"}, Result: "result two", Status: "succeeded", ExecutedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		},
	}}}
	turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: key, Generation: 1, UserText: "hello", AssistantText: "done", History: history, AssistantFinishReason: "stop", AssistantReasoning: "thinking trace", AssistantReasoningContent: "thinking trace", AssistantTokenCount: 42, UserPlatformMessageID: "msg-123", Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("exchange row count=%d err=%v", count, err)
	}
	type row struct {
		id      int64
		role    string
		content sql.NullString
		callID  sql.NullString
		calls   sql.NullString
		name    sql.NullString
		finish  sql.NullString
		reason  sql.NullString
		tokens  sql.NullInt64
		plat    sql.NullString
		ident   []byte
		order   sql.NullInt64
		active  int
	}
	var rows []row
	res, err := s.db.SQL().QueryContext(ctx, `SELECT id,role,content,tool_call_id,tool_calls,tool_name,finish_reason,reasoning,token_count,platform_message_id,display_identity,display_order,active FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
		var r row
		if err := res.Scan(&r.id, &r.role, &r.content, &r.callID, &r.calls, &r.name, &r.finish, &r.reason, &r.tokens, &r.plat, &r.ident, &r.order, &r.active); err != nil {
			res.Close()
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	res.Close()
	if err := res.Err(); err != nil {
		t.Fatal(err)
	}
	if rows[0].role != "user" || rows[0].plat.String != "msg-123" || rows[1].role != "assistant" || !rows[1].calls.Valid || rows[1].finish.String != "tool_calls" {
		t.Fatalf("assistant tool-request row malformed: %+v", rows[1])
	}
	if rows[2].role != "tool" || rows[2].callID.String != "call_provided_1" || rows[2].name.String != "web_search" || rows[2].content.String != "result one" {
		t.Fatalf("first tool row malformed: %+v", rows[2])
	}
	if rows[3].role != "tool" || rows[3].callID.String == "" || rows[3].callID.String == "call_provided_1" || rows[3].name.String != "memory" {
		t.Fatalf("fallback tool call id missing: %+v", rows[3])
	}
	if rows[4].id != turn.ID || rows[4].role != "assistant" || rows[4].finish.String != "stop" || rows[4].reason.String != "thinking trace" || !rows[4].tokens.Valid || rows[4].tokens.Int64 != 42 {
		t.Fatalf("final assistant row malformed: %+v", rows[4])
	}
	for _, r := range rows {
		if r.active != 0 {
			t.Fatal("pending exchange surfaced before delivery")
		}
		if len(r.ident) != 32 {
			t.Fatalf("display_identity is not a 32-byte hash: id=%d len=%d", r.id, len(r.ident))
		}
		if !r.order.Valid {
			t.Fatalf("display_order was not assigned: id=%d", r.id)
		}
	}
	seen := make(map[string]bool)
	for _, r := range rows {
		key := string(r.ident)
		if seen[key] {
			t.Fatalf("distinct transcript rows share display_identity: id=%d", r.id)
		}
		seen[key] = true
	}
	var msgCount, toolCount int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT message_count,tool_call_count FROM sessions`).Scan(&msgCount, &toolCount); err != nil || msgCount != 5 || toolCount != 2 {
		t.Fatalf("session counters msg=%d tool=%d err=%v", msgCount, toolCount, err)
	}
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE active=1`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("delivery did not publish exchange rows: count=%d err=%v", count, err)
	}
	if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'result'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("tool FTS text missing: count=%d err=%v", count, err)
	}
}

func TestProfileStoreOperationsEmitSafeDebugMeasurements(t *testing.T) {
	for _, level := range []config.Level{config.LevelDebug} {
		t.Run(level.String(), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			log := config.NewLogger(level)
			log.SetOutput(&output)
			s, err := NewProfileStore(context.Background(), root, "alice", log)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			key := "discord:dm:private-key-canary"
			if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.BindSessionFileMemory(ctx, "alice", key, 1, "private-memory-canary", ""); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "private-key-canary") || strings.Contains(output.String(), "private-memory-canary") {
				t.Fatal("private content entered logs")
			}
			for _, event := range []string{"memory.profile.session.complete", "memory.profile.snapshot.complete"} {
				if strings.Count(output.String(), `"event":"`+event+`"`) != 1 {
					t.Fatal("missing or duplicated measurement", event)
				}
			}
		})
	}
}

func TestNewSessionEndedConversationSurvivesSweepUntilTTL(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	now := time.Now()
	s.now = func() time.Time { return now }
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	appendProfileExchange(t, s, 1, "kept transcript")
	if err := s.NewSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	// A sweep before the TTL lapses must not delete the ended session.
	if err := s.SweepProfile(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("new-session transcript swept before TTL: count=%d err=%v", count, err)
	}
	// After the TTL lapses, ordinary expiry removes it.
	now = now.Add(2 * time.Hour)
	if err := s.SweepProfile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired transcript not swept: count=%d err=%v", count, err)
	}
}

func TestDeliveryIntoEndedSessionFailsClosed(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: key, Generation: 1, UserText: "racing send", AssistantText: "answer", Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	// The conversation moves on before the outbound send confirms.
	if err := s.NewSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("late delivery into an ended session published: %v", err)
	}
	var active int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE active=1`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("unconfirmed exchange surfaced: active=%d err=%v", active, err)
	}
}

func TestDisplayIdentityDedupeKey(t *testing.T) {
	content, callID, calls, name := "hello", "call_1", `[{"id":"call_1"}]`, "web_search"
	base := displayIdentity("assistant", &content, 1790430142.404224, &callID, &calls, &name)
	if len(base) != 32 {
		t.Fatalf("display_identity length=%d", len(base))
	}
	// Same tuple reproduces the digest; any key field changes it.
	if got := displayIdentity("assistant", &content, 1790430142.404224, &callID, &calls, &name); string(got) != string(base) {
		t.Fatal("dedupe key is not deterministic")
	}
	other := content + "!"
	if got := displayIdentity("assistant", &other, 1790430142.404224, &callID, &calls, &name); string(got) == string(base) {
		t.Fatal("content change preserved identity")
	}
	if got := displayIdentity("assistant", &content, 1790430142.404225, &callID, &calls, &name); string(got) == string(base) {
		t.Fatal("timestamp change preserved identity")
	}
	// NULL never collides with an empty string.
	empty := ""
	if got := displayIdentity("tool", &empty, 1, nil, nil, nil); len(got) != 32 {
		t.Fatal("empty identity malformed")
	} else if same := displayIdentity("tool", nil, 1, nil, nil, nil); string(same) == string(got) {
		t.Fatal("NULL content collided with empty content")
	}
}

func TestAppendWritesSessionRowAttributes(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Creation leaves transport ownership unset; the first append records the
	// receiving adapter instead of the runtime profile.
	created, err := s.ActiveSessionID(ctx, "alice", key, session.Generation)
	if err != nil {
		t.Fatal(err)
	}
	var createdTransport sql.NullString
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT transport_profile FROM sessions WHERE id=?`, created).Scan(&createdTransport); err != nil {
		t.Fatal(err)
	}
	if createdTransport.Valid {
		t.Fatalf("creation set transport profile: %q", createdTransport.String)
	}
	prompt := "rendered system prompt"
	sum := sha256.Sum256([]byte(prompt))
	hash := hex.EncodeToString(sum[:])
	turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: key, Generation: session.Generation,
		UserText: "hello", AssistantText: "done",
		Model: "test-model", BillingProvider: "custom", BillingBaseURL: "https://models.example/v1",
		ModelConfig: `{"gateway_runtime":{"provider":"custom","base_url":"https://models.example/v1","api_mode":"chat_completions"}}`,
		ChatID:      "channel-1", ChatType: "group", Platform: "discord", ChatDisplayName: "general",
		TransportProfile: "default", PlatformUserID: "+15550001111", PlatformDisplayName: "Alice",
		SystemPromptHash: hash, SystemPromptText: prompt, ActivityDescription: "answer",
		Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.ActiveSessionID(ctx, "alice", key, session.Generation)
	if err != nil {
		t.Fatal(err)
	}
	var model, modelConfig, billingProvider, billingBaseURL, chatID, chatType, platformUserID, platformDisplayName, transportProfile, promptHash, activity, provenance sql.NullString
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT model,model_config,billing_provider,billing_base_url,chat_id,chat_type,user_id,display_name,transport_profile,system_prompt_hash,last_activity_description,last_activity_provenance FROM sessions WHERE id=?`, id).Scan(
		&model, &modelConfig, &billingProvider, &billingBaseURL, &chatID, &chatType, &platformUserID, &platformDisplayName, &transportProfile, &promptHash, &activity, &provenance); err != nil {
		t.Fatal(err)
	}
	if model.String != "test-model" || billingProvider.String != "custom" || billingBaseURL.String != "https://models.example/v1" {
		t.Fatalf("model/billing mismatch: %+v", model)
	}
	if !json.Valid([]byte(modelConfig.String)) {
		t.Fatalf("model config is not JSON: %q", modelConfig.String)
	}
	if chatID.String != "channel-1" || chatType.String != "group" || platformUserID.String != "+15550001111" || platformDisplayName.String != "Alice" {
		t.Fatalf("chat identity mismatch: %q %q %q %q", chatID.String, chatType.String, platformUserID.String, platformDisplayName.String)
	}
	if transportProfile.String != "default" {
		t.Fatalf("transport profile mismatch: %q", transportProfile.String)
	}
	if promptHash.String != hash || activity.String != "answer" || provenance.String != "unknown" {
		t.Fatalf("prompt/activity mismatch: %q %q %q", promptHash.String, activity.String, provenance.String)
	}
	var stored string
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT prompt FROM system_prompts WHERE hash=?`, hash).Scan(&stored); err != nil || stored != prompt {
		t.Fatalf("system prompt not ledgered: %v", err)
	}
	var origin string
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT origin_json FROM sessions WHERE id=?`, id).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(origin), &record); err != nil {
		t.Fatalf("origin is not JSON: %v", err)
	}
	// Generation fencing survives the merge; platform identity joins it.
	if record["version"] != float64(1) || record["generation"] != float64(session.Generation) || record["ttl_seconds"] == nil {
		t.Fatalf("fence fields lost: %s", origin)
	}
	want := map[string]string{"platform": "discord", "chat_id": "channel-1", "chat_name": "general", "chat_type": "group", "user_id": "+15550001111", "user_name": "Alice", "profile": "alice"}
	for key, value := range want {
		if record[key] != value {
			t.Fatalf("origin[%s]=%v want %q: %s", key, record[key], value, origin)
		}
	}
	// A second turn reuses the prompt row instead of duplicating it.
	if _, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: key, Generation: session.Generation,
		UserText: "again", AssistantText: "done", Model: "test-model", SystemPromptHash: hash, SystemPromptText: prompt,
		Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}}); err != nil {
		t.Fatal(err)
	}
	var promptRows int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM system_prompts`).Scan(&promptRows); err != nil || promptRows != 1 {
		t.Fatalf("prompt rows=%d err=%v", promptRows, err)
	}
	bad := SessionTurnWrite{UserID: "alice", SessionID: key, Generation: session.Generation,
		UserText: "bad", AssistantText: "done", Model: "m", TransportProfile: "not a profile!",
		Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}}
	if _, err := s.AppendPendingSessionTurn(ctx, bad); err == nil {
		t.Fatal("invalid transport profile accepted")
	}
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRefreshesSessionLedger(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	history := ToolHistory{Version: ToolHistoryVersion, Batches: []ToolHistoryBatch{{Calls: []ToolHistoryCall{
		{Name: "web_search", ProviderCallID: "call-1", Arguments: map[string]interface{}{}, Result: "r1", Status: "succeeded", ExecutedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		{Name: "memory", ProviderCallID: "call-2", Arguments: map[string]interface{}{}, Result: "r2", Status: "succeeded", ExecutedAt: time.Now().UTC().Format(time.RFC3339Nano)},
	}}}}
	turn, err := s.AppendPendingSessionTurn(ctx, SessionTurnWrite{UserID: "alice", SessionID: key, Generation: session.Generation,
		UserText: "hello", AssistantText: "done", History: history, Model: "m",
		Pressure: SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.ActiveSessionID(ctx, "alice", key, session.Generation)
	if err != nil {
		t.Fatal(err)
	}
	usage := ModelUsageRecord{SessionID: key, UserID: "alice", Generation: session.Generation, Model: "m", ApiCalls: 2, PromptTokens: 100, CompletionTokens: 10}
	if err := s.RecordModelUsage(ctx, usage); err != nil {
		t.Fatal(err)
	}
	compression := usage
	compression.Task = "compression"
	compression.PromptTokens, compression.CompletionTokens = 50, 5
	if err := s.RecordModelUsage(ctx, compression); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	var input, output, calls int
	var toolSet sql.NullString
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT input_tokens,output_tokens,api_call_count,tool_names FROM sessions WHERE id=?`, id).Scan(&input, &output, &calls, &toolSet); err != nil {
		t.Fatal(err)
	}
	if input != 100 || output != 10 || calls != 2 {
		t.Fatalf("ledger rollup counted non-chat tasks: in=%d out=%d calls=%d", input, output, calls)
	}
	encoded, _ := json.Marshal([]string{"memory", "web_search"})
	sum := sha256.Sum256(encoded)
	if toolSet.String != hex.EncodeToString(sum[:]) {
		t.Fatalf("tool set hash mismatch: %q", toolSet.String)
	}
}

func TestNewSessionLinksParent(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:123"
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	first, err := s.ActiveSessionID(ctx, "alice", key, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.NewSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if session.Generation != 2 {
		t.Fatalf("generation=%d", session.Generation)
	}
	second, err := s.ActiveSessionID(ctx, "alice", key, 2)
	if err != nil {
		t.Fatal(err)
	}
	var parent sql.NullString
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT parent_session_id FROM sessions WHERE id=?`, second).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	if parent.String != first {
		t.Fatalf("parent=%q want %q", parent.String, first)
	}
}
