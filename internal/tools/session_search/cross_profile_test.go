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

func newPeerFixture(t *testing.T, profile string) *memory.ProfileStore {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.NewProfileStore(context.Background(), root, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func deliverAs(t *testing.T, store *memory.ProfileStore, user, key, userText, answer string) int64 {
	t.Helper()
	session, err := store.ResolveSessionContext(context.Background(), store.Profile(), key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.AppendPendingSessionTurn(context.Background(), memory.SessionTurnWrite{UserID: user, SessionID: key, Generation: session.Generation, UserText: userText, AssistantText: answer, Pressure: memory.SessionPromptPressure{Tokens: 1, Limit: 100, Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionTurnDelivered(context.Background(), user, turn.ID); err != nil {
		t.Fatal(err)
	}
	return turn.ID
}

func groupContext(ctx context.Context, key string, generation int, gateway, chat string) context.Context {
	return requestctx.WithMetadata(ctx, requestctx.Metadata{SessionID: key, SessionGeneration: generation, GroupGateway: gateway, GroupChatID: chat})
}

func decodeResults(t *testing.T, content string) []map[string]interface{} {
	t.Helper()
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(content), &decoded); err != nil {
		t.Fatalf("invalid JSON envelope: %v", err)
	}
	raw, _ := decoded["results"].([]interface{})
	out := make([]map[string]interface{}, 0, len(raw))
	for _, entry := range raw {
		item, _ := entry.(map[string]interface{})
		out = append(out, item)
	}
	return out
}

func TestCrossProfileDiscoverySharesGroupOnly(t *testing.T) {
	alice := newPeerFixture(t, "alice")
	bob := newPeerFixture(t, "bob")
	peers := map[string]*memory.ProfileStore{"alice": alice, "bob": bob}

	const groupKey = "imessage:chat123:userA"
	const peerGroupKey = "imessage:chat123:userB"
	deliverAs(t, alice, "alice", groupKey, "live opener", "noted")
	deliverAs(t, alice, "alice", "imessage:chat999:userA", "campsite reservation friday", "noted")
	deliverAs(t, bob, "bob", peerGroupKey, "campsite friday confirmed", "acked")
	deliverAs(t, bob, "bob", "imessage:dm:userB", "campsite secret dm", "acked")
	deliverAs(t, bob, "bob", "imessage:otherchat:userB", "campsite other group", "acked")

	principal := identity.Principal{CanonicalUserID: "alice", Gateway: "imessage", ExternalID: "userA", Assurance: identity.AssuranceBlueBubblesWebhook}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	session, err := alice.ResolveSessionContext(context.Background(), "alice", groupKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx = groupContext(ctx, groupKey, session.Generation, "imessage", "chat123")

	result, err := NewHandler(alice, peers)(ctx, map[string]interface{}{"query": "campsite"})
	if err != nil {
		t.Fatal(err)
	}
	results := decodeResults(t, result.Content)
	if len(results) != 2 {
		t.Fatalf("expected own + peer group hits, got %d: %s", len(results), result.Content)
	}
	profiles := map[string]bool{}
	for _, item := range results {
		profile, _ := item["profile"].(string)
		profiles[profile] = true
		link, _ := item["link"].(string)
		if !strings.HasPrefix(link, "@session:"+profile+"/") {
			t.Fatalf("link %q does not match profile %q", link, profile)
		}
	}
	if !profiles["alice"] || !profiles["bob"] {
		t.Fatalf("missing profile in results: %v", profiles)
	}
	for _, item := range results {
		snippet, _ := item["snippet"].(string)
		if strings.Contains(snippet, "secret dm") || strings.Contains(snippet, "other group") {
			t.Fatalf("out-of-scope peer message leaked: %+v", item)
		}
	}
}

func TestCrossProfileReadScrollEnforced(t *testing.T) {
	alice := newPeerFixture(t, "alice")
	bob := newPeerFixture(t, "bob")
	peers := map[string]*memory.ProfileStore{"alice": alice, "bob": bob}

	const groupKey = "discord:chan1:userA"
	const peerGroupKey = "discord:chan1:userB"
	anchor := deliverAs(t, bob, "bob", peerGroupKey, "group needle here", "peer answer")
	dmTurn := deliverAs(t, bob, "bob", "discord:dm:userB", "dm needle here", "peer answer")
	_ = dmTurn
	other := deliverAs(t, bob, "bob", "discord:chan2:userB", "other needle here", "peer answer")
	_ = other

	peerID, err := bob.ActiveSessionID(context.Background(), "bob", peerGroupKey, mustGeneration(t, bob, peerGroupKey))
	if err != nil || peerID == "" {
		t.Fatalf("peer session id: %v", err)
	}
	dmID, err := bob.ActiveSessionID(context.Background(), "bob", "discord:dm:userB", mustGeneration(t, bob, "discord:dm:userB"))
	if err != nil || dmID == "" {
		t.Fatalf("peer dm id: %v", err)
	}
	otherID, err := bob.ActiveSessionID(context.Background(), "bob", "discord:chan2:userB", mustGeneration(t, bob, "discord:chan2:userB"))
	if err != nil || otherID == "" {
		t.Fatalf("peer other id: %v", err)
	}

	principal := identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "userA", Assurance: identity.AssuranceDiscordGateway}
	ctx := requestctx.WithPrincipal(context.Background(), principal)
	live, err := alice.ResolveSessionContext(context.Background(), "alice", groupKey, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	deliverAs(t, alice, "alice", groupKey, "live opener", "live answer")
	ctx = groupContext(ctx, groupKey, live.Generation, "discord", "chan1")
	handler := NewHandler(alice, peers)

	read, err := handler(ctx, map[string]interface{}{"session_id": peerID, "profile": "bob"})
	if err != nil {
		t.Fatalf("peer group read rejected: %v", err)
	}
	if !strings.Contains(read.Content, "group needle here") {
		t.Fatalf("peer group read missing content: %s", read.Content)
	}
	scroll, err := handler(ctx, map[string]interface{}{"session_id": peerID, "profile": "bob", "around_message_id": float64(anchor)})
	if err != nil {
		t.Fatalf("peer group scroll rejected: %v", err)
	}
	if !strings.Contains(scroll.Content, "group needle here") {
		t.Fatalf("peer group scroll missing content: %s", scroll.Content)
	}
	for name, args := range map[string]map[string]interface{}{
		"peer dm":     {"session_id": dmID, "profile": "bob"},
		"other group": {"session_id": otherID, "profile": "bob"},
		"unknown":     {"session_id": peerID, "profile": "mallory"},
	} {
		if _, err := handler(ctx, args); err == nil {
			t.Fatalf("%s read accepted", name)
		}
	}

	dmCtx := requestctx.WithPrincipal(context.Background(), principal)
	if _, err := handler(dmCtx, map[string]interface{}{"session_id": peerID, "profile": "bob"}); err == nil {
		t.Fatal("cross-profile read accepted without group context")
	}
	if _, err := handler(ctx, map[string]interface{}{"profile": "bob"}); err == nil {
		t.Fatal("profile without session_id accepted")
	}
}

func mustGeneration(t *testing.T, store *memory.ProfileStore, key string) int {
	t.Helper()
	session, err := store.ResolveSessionContext(context.Background(), store.Profile(), key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return session.Generation
}
