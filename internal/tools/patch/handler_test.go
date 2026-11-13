package patch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

func testContext(requestID string) context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID})
	return ctx
}

func TestDefinitionContract(t *testing.T) {
	def := Definition()
	if def.Name != Name || def.Name == "" || def.Description == "" {
		t.Fatalf("invalid definition: %+v", def)
	}
	if def.Parameters.Type != "object" || def.Parameters.Properties == nil {
		t.Fatalf("invalid parameters: %+v", def.Parameters)
	}
}

func TestPatchUniqueAndReplaceAll(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	ctx := testContext("req-p")
	if err := os.WriteFile(filepath.Join(root, "p.txt"), []byte("one\ntwo\none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch := NewPatchHandler(ws)
	if _, err := patch(ctx, map[string]interface{}{"path": "p.txt", "old_string": "one", "new_string": "1"}); err == nil {
		t.Fatal("ambiguous patch succeeded")
	}
	res, err := patch(ctx, map[string]interface{}{"path": "p.txt", "old_string": "one", "new_string": "1", "replace_all": true})
	if err != nil || !strings.Contains(res.Content, "2 occurrences") {
		t.Fatalf("replace_all = %+v %v", res, err)
	}
}
