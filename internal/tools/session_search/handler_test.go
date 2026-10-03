package session_search

import (
	"context"
	"encoding/json"
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
