package filememory

import (
	"context"
	"os"
	"path/filepath"
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
	if err != nil || result.Content != "updated\n§\nanother" {
		t.Fatalf("write: %+v %v", result, err)
	}
	_, memory, err := store.Read(ctx, "one")
	if err != nil || memory != result.Content {
		t.Fatalf("store: %q %v", memory, err)
	}
	for _, args := range []map[string]interface{}{
		{"action": "read", "target": "memory"},
		{"action": "add", "content": "missing target"},
		{"action": "replace", "target": "memory", "old_text": "updated", "content": "one", "new_text": "two"},
		{"action": "replace", "target": "memory", "old_text": "updated", "content": "same", "new_text": "same"},
		{"action": "add", "target": "memory", "new_text": "not an add alias"},
		{"action": "remove", "target": "memory", "old_text": "updated", "new_text": "not a remove alias"},
		{"action": "replace", "target": "memory", "old_text": "updated", "new_text": 42},
		{"action": "add", "target": "memory", "operations": []interface{}{map[string]interface{}{"action": "add", "content": "wrong"}}},
		{"target": "memory", "operations": []interface{}{map[string]interface{}{"action": "add", "content": 5}}},
		{"target": "memory", "operations": []interface{}{map[string]interface{}{"action": "replace", "old_text": "updated", "new_text": "one", "content": "two"}}},
		{"target": "memory", "operations": []interface{}{map[string]interface{}{"action": "add", "content": "safe"}, map[string]interface{}{"action": "remove", "old_text": "missing"}}},
	} {
		if _, err := handler(ctx, args); err == nil {
			t.Fatalf("accepted invalid args: %#v", args)
		}
	}
	_, memory, err = store.Read(ctx, "one")
	if err != nil || memory != result.Content {
		t.Fatalf("invalid call changed stored memory: %q %v", memory, err)
	}
}
