package filememory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory/files"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestHandlerRequiresPrincipalAndValidatesArguments(t *testing.T) {
	root := t.TempDir()
	store := files.NewStore(root)
	handler := NewHandler(store)
	if _, err := handler(context.Background(), map[string]interface{}{"target": "memory", "action": "add", "content": "note"}); err == nil {
		t.Fatal("anonymous write")
	}
	if _, err := os.Stat(filepath.Join(root, "one")); !os.IsNotExist(err) {
		t.Fatalf("anonymous write created files: %v", err)
	}
	ctx := requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "one", Gateway: "discord", ExternalID: "remote", Assurance: identity.AssuranceDiscordGateway})
	untrusted := requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "two", Gateway: "discord", ExternalID: "remote", Assurance: identity.AssuranceSelfAsserted})
	if _, err := handler(untrusted, map[string]interface{}{"target": "memory", "action": "add", "content": "note"}); err == nil {
		t.Fatal("self-asserted write")
	}
	if _, err := os.Stat(filepath.Join(root, "two")); !os.IsNotExist(err) {
		t.Fatalf("self-asserted write created files: %v", err)
	}
	if _, err := handler(ctx, map[string]interface{}{"action": "add", "target": "memory", "content": "a note"}); err != nil {
		t.Fatal(err)
	}
	result, err := handler(ctx, map[string]interface{}{"target": "memory", "operations": []interface{}{map[string]interface{}{"action": "replace", "old_text": "note", "new_text": "updated"}, map[string]interface{}{"action": "add", "content": "another"}}})
	if err != nil || result.Content != "Memory updated (current/limit chars: 17/2200).\nupdated\n§\nanother" {
		t.Fatalf("write: %+v %v", result, err)
	}
	_, memory, err := store.Read(ctx, "one")
	if err != nil || memory != "updated\n§\nanother" {
		t.Fatalf("store: %q %v", memory, err)
	}
	result, err = handler(ctx, map[string]interface{}{"target": "memory", "operations": []interface{}{
		map[string]interface{}{"action": "replace", "old_text": "updated", "content": "preferred", "new_text": "ignored"},
		map[string]interface{}{"action": "remove", "old_text": "another"},
		map[string]interface{}{"action": "add", "content": "new"},
	}})
	if err != nil || result.Content != "Memory updated (current/limit chars: 15/2200).\npreferred\n§\nnew" {
		t.Fatalf("content precedence and batch: %+v %v", result, err)
	}
	for _, args := range []map[string]interface{}{
		{"action": "read", "target": "memory"},
		{"action": "add", "content": "missing target"},
		{"action": "replace", "target": "memory", "old_text": "preferred", "content": "one", "new_text": 42},
		{"action": "add", "target": "memory", "new_text": "not an add alias"},
		{"action": "remove", "target": "memory", "old_text": "updated", "new_text": "not a remove alias"},
		{"action": "replace", "target": "memory", "old_text": "preferred", "new_text": 42},
		{"action": "add", "target": "memory", "operations": []interface{}{map[string]interface{}{"action": "add", "content": "wrong"}}},
		{"target": "memory", "operations": []interface{}{map[string]interface{}{"action": "add", "content": 5}}},
		{"target": "memory", "operations": []interface{}{map[string]interface{}{"action": "replace", "old_text": "missing", "new_text": "one", "content": "two"}}},
		{"target": "memory", "operations": []interface{}{map[string]interface{}{"action": "add", "content": "safe"}, map[string]interface{}{"action": "remove", "old_text": "missing"}}},
	} {
		if _, err := handler(ctx, args); err == nil {
			t.Fatalf("accepted invalid args: %#v", args)
		}
	}
	_, memory, err = store.Read(ctx, "one")
	if err != nil || memory != "preferred\n§\nnew" {
		t.Fatalf("invalid call changed stored memory: %q %v", memory, err)
	}
	_, err = handler(ctx, map[string]interface{}{"target": "memory", "action": "add", "content": strings.Repeat("x", 2200)})
	if err == nil || !strings.Contains(err.Error(), "preferred\n§\nnew") || !strings.Contains(err.Error(), "15/2200 chars") {
		t.Fatalf("full memory did not show current entries: %v", err)
	}
	result, err = handler(ctx, map[string]interface{}{"target": "memory", "operations": []interface{}{
		map[string]interface{}{"action": "remove", "old_text": "preferred"},
		map[string]interface{}{"action": "replace", "old_text": "new", "new_text": strings.Repeat("界", 2200)},
	}})
	if err != nil || !strings.Contains(result.Content, "2200/2200") {
		t.Fatalf("final-only capacity: %+v %v", result, err)
	}
}
