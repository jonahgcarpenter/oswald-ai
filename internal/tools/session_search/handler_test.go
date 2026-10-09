package session_search

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func newSearchFixture(t *testing.T) (*memory.ProfileStore, context.Context) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewProfileStore(context.Background(), root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	principal := identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	return store, ctx
}

func deliver(t *testing.T, store *memory.ProfileStore, generation int, key, user, answer string) int64 {
	t.Helper()
	ctx := context.Background()
	turn, err := store.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{UserID: "alice", SessionID: key, Generation: generation, UserText: user, AssistantText: answer, Pressure: memory.SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}

func run(t *testing.T, store *memory.ProfileStore, ctx context.Context, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	result, err := NewHandler(store)(ctx, args)
	if err != nil {
		t.Fatalf("session_search: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil {
		t.Fatalf("invalid JSON envelope: %v\n%s", err, result.Content)
	}
	return decoded
}

func TestDiscoverAdaptiveHydration(t *testing.T) {
	store, ctx := newSearchFixture(t)
	generation := resolveGeneration(t, store)
	deliver(t, store, generation, "discord:dm:123", "where did the docker deploy fail", "the image tag was stale")
	deliver(t, store, generation, "discord:dm:123", "another docker topic", "docker build cache")

	envelope := run(t, store, ctx, map[string]interface{}{"query": "docker"})
	if envelope["mode"] != "discover" || envelope["success"] != true {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	results, _ := envelope["results"].([]interface{})
	if len(results) == 0 {
		t.Fatal("no discovery results")
	}
	top, _ := results[0].(map[string]interface{})
	if top["detail"] != "full" || top["match_message_id"] == nil || top["link"] == nil {
		t.Fatalf("top result not hydrated: %+v", top)
	}
	if link, _ := top["link"].(string); !strings.HasPrefix(link, "@session:alice/") {
		t.Fatalf("unexpected link: %s", link)
	}
	for _, raw := range results[1:] {
		entry, _ := raw.(map[string]interface{})
		if entry["detail"] != "compact" {
			t.Fatalf("lower result should be compact: %+v", entry)
		}
	}
}

func TestScrollReadBrowseShapes(t *testing.T) {
	store, ctx := newSearchFixture(t)
	generation := resolveGeneration(t, store)
	var anchor int64
	for i := 0; i < 6; i++ {
		anchor = deliver(t, store, generation, "discord:dm:123", "user message", "assistant reply")
	}
	sessionID := sessionIDForKey(t, store, "discord:dm:123")

	scroll := run(t, store, ctx, map[string]interface{}{"session_id": sessionID, "around_message_id": float64(anchor), "window": float64(2)})
	if scroll["mode"] != "scroll" || scroll["around_message_id"] == nil {
		t.Fatalf("scroll failed: %+v", scroll)
	}
	read := run(t, store, ctx, map[string]interface{}{"session_id": sessionID})
	if read["mode"] != "read" || read["message_count"] == nil {
		t.Fatalf("read failed: %+v", read)
	}
	browse := run(t, store, ctx, map[string]interface{}{})
	if browse["mode"] != "browse" || browse["count"].(float64) < 1 {
		t.Fatalf("browse failed: %+v", browse)
	}
}

func TestUnknownArgumentAndForeignProfileRejected(t *testing.T) {
	store, ctx := newSearchFixture(t)
	if _, err := NewHandler(store)(ctx, map[string]interface{}{"bogus": "x"}); err == nil {
		t.Fatal("unknown argument accepted")
	}
	bob := identity.Principal{CanonicalUserID: "bob", Gateway: "discord", ExternalID: "1", Assurance: identity.AssuranceDiscordGateway}
	if _, err := NewHandler(store)(requestctx.WithPrincipal(context.Background(), bob), map[string]interface{}{"query": "x"}); err == nil {
		t.Fatal("foreign profile accepted")
	}
	if _, err := NewHandler(store)(context.Background(), map[string]interface{}{"query": "x"}); err == nil {
		t.Fatal("unauthenticated call accepted")
	}
}

func TestDiscoverNoHitCoachesSyntax(t *testing.T) {
	store, ctx := newSearchFixture(t)
	resolveGeneration(t, store)
	deliver(t, store, resolveGeneration(t, store), "discord:dm:123", "hello", "world")
	envelope := run(t, store, ctx, map[string]interface{}{"query": "nonexistentterm"})
	if envelope["count"].(float64) != 0 || envelope["message"] == nil {
		t.Fatalf("no-hit mismatch: %+v", envelope)
	}
}

func resolveGeneration(t *testing.T, store *memory.ProfileStore) int {
	t.Helper()
	ctx := context.Background()
	session, err := store.ResolveSessionContext(ctx, "alice", "discord:dm:123", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return session.Generation
}

func sessionIDForKey(t *testing.T, store *memory.ProfileStore, key string) string {
	t.Helper()
	id, err := store.ActiveSessionID(context.Background(), "alice", key, resolveGeneration(t, store))
	if err != nil || id == "" {
		t.Fatalf("active session id: %v", err)
	}
	return id
}

func TestEnvelopeStaysBoundedAndValidJSON(t *testing.T) {
	store, ctx := newSearchFixture(t)
	generation := resolveGeneration(t, store)
	// Large per-message content stresses the whole-envelope budget.
	large := strings.Repeat("docker ", 2000)
	for i := 0; i < 6; i++ {
		deliver(t, store, generation, "discord:dm:123", large, large)
	}
	result, err := NewHandler(store)(ctx, map[string]interface{}{"query": "docker", "limit": float64(10), "detail": "full"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(result.Content)); got > envelopeRunes {
		t.Fatalf("envelope exceeded budget: %d runes", got)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil {
		t.Fatalf("envelope is not valid JSON: %v", err)
	}
}

func TestRoleFilterAndTemporalBounds(t *testing.T) {
	store, ctx := newSearchFixture(t)
	generation := resolveGeneration(t, store)
	deliver(t, store, generation, "discord:dm:123", "alpha topic", "beta reply")
	// An empty role set is impossible; discovery defaults to user,assistant.
	envelope := run(t, store, ctx, map[string]interface{}{"query": "alpha", "role_filter": "user"})
	if envelope["count"].(float64) != 1 {
		t.Fatalf("user-only filter mismatch: %+v", envelope)
	}
	// A before bound of 1h excludes messages from the last hour.
	envelope = run(t, store, ctx, map[string]interface{}{"query": "alpha", "before": "1h"})
	if envelope["count"].(float64) != 0 {
		t.Fatalf("before bound mismatch: %+v", envelope)
	}
	if _, err := NewHandler(store)(ctx, map[string]interface{}{"query": "alpha", "after": "not-a-date"}); err == nil {
		t.Fatal("invalid temporal bound accepted")
	}
}

func TestDiscoveryReturnsBothSidesAndReadIncludesNativeTraces(t *testing.T) {
	store, ctx := newSearchFixture(t)
	generation := resolveGeneration(t, store)
	key := "discord:dm:123"
	deliver(t, store, generation, key, "opening", "opening answer")
	trace := memory.ToolHistory{Version: memory.ToolHistoryVersion, Batches: []memory.ToolHistoryBatch{{Calls: []memory.ToolHistoryCall{{Name: "web_search", HistoryMode: "full", Arguments: map[string]interface{}{"query": "deployment"}, Result: "toolneedle connection refused", Status: "succeeded", SearchResult: true}}}}}
	turn, err := store.AppendPendingSessionTurn(ctx, memory.SessionTurnWrite{UserID: "alice", SessionID: key, Generation: generation, UserText: "matchneedle", AssistantText: "answer", History: trace, Pressure: memory.SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(ctx, "alice", turn.ID); err != nil {
		t.Fatal(err)
	}
	deliver(t, store, generation, key, "following", "following answer")
	id := sessionIDForKey(t, store, key)
	for _, args := range []map[string]interface{}{
		{"query": "matchneedle", "role_filter": "user"},
		{"query": "toolneedle", "role_filter": "tool"},
		{"session_id": id},
		{"session_id": id, "around_message_id": turn.ID},
	} {
		result, err := NewHandler(store)(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"opening", "following answer", "tool_history", "toolneedle connection refused", "web_search", "deployment"} {
			if !strings.Contains(result.Content, want) {
				t.Fatalf("%v missing %q: %s", args, want, result.Content)
			}
		}
	}
	if result := run(t, store, ctx, map[string]interface{}{"query": "toolneedle"}); result["count"].(float64) != 0 {
		t.Fatal("tool-only text matched default roles")
	}
	if result := run(t, store, ctx, map[string]interface{}{"query": "matchneedle", "role_filter": "assistant"}); result["count"].(float64) != 0 {
		t.Fatal("assistant-only filter matched user content")
	}
	// These counts represent messages outside the returned window, not the
	// number already included on each side of the anchor.
	scroll := run(t, store, ctx, map[string]interface{}{"session_id": id, "around_message_id": turn.ID, "window": 1})
	if scroll["messages_before"].(float64) != 2 || scroll["messages_after"].(float64) != 1 {
		t.Fatalf("incorrect remainder counts: %+v", scroll)
	}
	messages := scroll["messages"].([]interface{})
	next := messages[len(messages)-1].(map[string]interface{})["id"]
	forward := run(t, store, ctx, map[string]interface{}{"session_id": id, "around_message_id": next, "window": 1})
	if forward["around_message_id"] != next {
		t.Fatal("scroll pagination lost boundary anchor")
	}
}

func TestSearchRejectsInvalidArgumentTypesAndRoleFilters(t *testing.T) {
	store, ctx := newSearchFixture(t)
	for _, args := range []map[string]interface{}{
		{"query": 1}, {"session_id": ""}, {"session_id": 1},
		{"query": "docker", "role_filter": "reasoning"},
		{"query": "docker", "role_filter": "user,bogus"},
		{"query": "docker", "after": "999999999999h"},
		{"query": "docker", "after": "2026-06-02", "before": "2026-06-01"},
		{"window": 1.5}, {"around_message_id": math.NaN()},
		{"limit": math.Inf(1)}, {"profile": "bob"},
	} {
		if _, err := NewHandler(store)(ctx, args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestLargeReadAndScrollKeepModeAndNavigation(t *testing.T) {
	store, ctx := newSearchFixture(t)
	generation := resolveGeneration(t, store)
	large := strings.Repeat("界", 6000)
	var anchor int64
	for i := 0; i < 15; i++ {
		id := deliver(t, store, generation, "discord:dm:123", large, large)
		if i == 7 {
			anchor = id
		}
	}
	id := sessionIDForKey(t, store, "discord:dm:123")
	for _, args := range []map[string]interface{}{{"session_id": id}, {"session_id": id, "around_message_id": anchor, "window": 20}} {
		result, err := NewHandler(store)(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if len([]rune(result.Content)) > envelopeRunes {
			t.Fatal("oversized envelope")
		}
		var response struct {
			Mode      string        `json:"mode"`
			Truncated bool          `json:"truncated"`
			Messages  []messageJSON `json:"messages"`
		}
		if err := json.Unmarshal([]byte(result.Content), &response); err != nil {
			t.Fatal(err)
		}
		if response.Mode == "truncated" || !response.Truncated || len(response.Messages) == 0 {
			t.Fatal("oversized result discarded its mode/navigation")
		}
		foundAnchor := false
		for _, message := range response.Messages {
			if !message.ContentTruncated || message.OriginalContentChars != 6000 {
				t.Fatal("lost original Unicode content size")
			}
			foundAnchor = foundAnchor || message.Anchor && message.ID == anchor
		}
		if response.Mode == "scroll" && !foundAnchor {
			t.Fatal("size bounding removed scroll anchor")
		}
	}
}
