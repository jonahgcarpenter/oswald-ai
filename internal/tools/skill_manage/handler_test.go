package skill_manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
)

func skillContext() context.Context {
	return requestctx.WithPrincipal(context.Background(), identity.Principal{CanonicalUserID: "tester", Gateway: "discord", ExternalID: "remote", Assurance: identity.AssuranceDiscordGateway})
}

func seedManaged(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "review")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\n\nFollow the checklist.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestHandlerRequiresPrincipal(t *testing.T) {
	handler := NewHandler(t.TempDir())
	if _, err := handler(context.Background(), map[string]interface{}{"action": "list"}); err == nil {
		t.Fatal("expected principal error")
	}
}

func TestHandlerRejectsUnknownArgs(t *testing.T) {
	handler := NewHandler(seedManaged(t))
	if _, err := handler(skillContext(), map[string]interface{}{"action": "list", "bogus": true}); err == nil {
		t.Fatal("expected unknown argument error")
	}
	if _, err := handler(skillContext(), map[string]interface{}{"action": "dance"}); err == nil {
		t.Fatal("expected invalid action error")
	}
}

func TestListAndViewShapes(t *testing.T) {
	handler := NewHandler(seedManaged(t))
	result, err := handler(skillContext(), map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "review") || !strings.Contains(result.Content, "Review code") {
		t.Fatalf("unexpected list content %q", result.Content)
	}
	viewed, err := handler(skillContext(), map[string]interface{}{"action": "view", "name": "review"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(viewed.Content, "Follow the checklist.") {
		t.Fatalf("unexpected view content %q", viewed.Content)
	}
	if _, err := handler(skillContext(), map[string]interface{}{"action": "view", "name": "missing"}); err == nil {
		t.Fatal("expected unknown skill error")
	}
	if _, err := handler(skillContext(), map[string]interface{}{"action": "view"}); err == nil {
		t.Fatal("expected missing name error")
	}
}

func TestListEmpty(t *testing.T) {
	handler := NewHandler(t.TempDir())
	result, err := handler(skillContext(), map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "no skills") {
		t.Fatalf("unexpected empty list %q", result.Content)
	}
}

func TestCreateAndUpdateThroughTool(t *testing.T) {
	root := t.TempDir()
	handler := NewHandler(root)
	if _, err := handler(skillContext(), map[string]interface{}{"action": "create", "name": "fresh", "description": "Fresh things", "content": "Step one."}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "fresh", "SKILL.md")); err != nil {
		t.Fatalf("skill file not created: %v", err)
	}
	// Direct file-tool writes are denied by the system group, but the tool
	// path bypasses workspace groups by design.
	if _, err := handler(skillContext(), map[string]interface{}{"action": "update", "name": "fresh", "old_text": "Step one.", "content": "Step one, then two."}); err != nil {
		t.Fatal(err)
	}
	viewed, err := handler(skillContext(), map[string]interface{}{"action": "view", "name": "fresh"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(viewed.Content, "then two") {
		t.Fatalf("unexpected updated content %q", viewed.Content)
	}
}

func TestCreateWithCategoryThroughTool(t *testing.T) {
	root := t.TempDir()
	handler := NewHandler(root)
	result, err := handler(skillContext(), map[string]interface{}{"action": "create", "name": "ship", "description": "Ship fast", "content": "Ship it.", "category": "code"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"ship"`) || !strings.Contains(result.Content, `"code"`) {
		t.Fatalf("unexpected create content %q", result.Content)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "code", "ship", "SKILL.md")); err != nil {
		t.Fatalf("categorized skill file missing: %v", err)
	}
	listed, err := handler(skillContext(), map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.Content, "ship [code]") {
		t.Fatalf("list missing categorized skill %q", listed.Content)
	}
	if _, err := handler(skillContext(), map[string]interface{}{"action": "create", "name": "evil", "description": "Evil", "content": "Body.", "category": "../escape"}); err == nil {
		t.Fatal("expected traversal category to fail")
	}
}

func TestDefinitionContract(t *testing.T) {
	def := Definition()
	if def.Name != Name || def.Parameters.Type != "object" {
		t.Fatalf("unexpected definition: %+v", def)
	}
	if len(def.Parameters.Required) != 1 || def.Parameters.Required[0] != "action" {
		t.Fatalf("unexpected required: %v", def.Parameters.Required)
	}
	for _, key := range []string{"action", "name", "file_path", "description", "content", "old_text", "category"} {
		if _, ok := def.Parameters.Properties[key]; !ok {
			t.Fatalf("missing property %q", key)
		}
	}
}
