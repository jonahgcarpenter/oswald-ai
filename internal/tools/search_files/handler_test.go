package searchfiles

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

func TestSearchFilesContentAndNames(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	ctx := testContext("req-s")
	if err := os.WriteFile(filepath.Join(root, "hello.py"), []byte("print('hi')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	search := NewSearchFilesHandler(ws)
	res, err := search(ctx, map[string]interface{}{"pattern": "print", "target": "content", "path": "."})
	if err != nil || !strings.Contains(res.Content, "hello.py") {
		t.Fatalf("content search = %+v %v", res, err)
	}
	res, err = search(ctx, map[string]interface{}{"pattern": "*.py", "target": "files", "path": "."})
	if err != nil || !strings.Contains(res.Content, "hello.py") {
		t.Fatalf("file search = %+v %v", res, err)
	}
}
