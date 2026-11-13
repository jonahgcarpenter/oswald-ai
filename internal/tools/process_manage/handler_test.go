package processmanage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

func testContext(requestID string) context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID})
	return ctx
}

func profilePair(t *testing.T) (alice, bob string) {
	t.Helper()
	tree := filepath.Join(t.TempDir(), ".oswald")
	alice = filepath.Join(tree, "profiles", "alice")
	bob = filepath.Join(tree, "profiles", "bob")
	for _, d := range []string{tree, alice, bob} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bob, "SOUL.md"), []byte("bob soul\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	return alice, bob
}

func sessionFrom(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "session_id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
		}
	}
	return ""
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

// Cross-profile stdin stays confined: writing another profile's files
// through the manager fails closed.
func TestProcessManageWriteConfined(t *testing.T) {
	alice, bob := profilePair(t)
	ws := workspace.NewWorkspace(alice, "alice")
	manage := NewProcessManageHandler(ws, process.NewManager(alice))
	_, err := manage(testContext("req-xwrite"), map[string]interface{}{
		"action":     "write",
		"session_id": "nosuch",
		"data":       "echo x > " + filepath.Join(bob, "notes.md"),
	})
	if err == nil || !strings.Contains(err.Error(), "system-managed") {
		t.Fatalf("write allowed cross-profile data: %v", err)
	}
}

// Background spawn through the terminal handler resolves in the manage
// handler's wait: the two surfaces share the same process manager.
func TestTerminalBackgroundRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	pm := process.NewManager(root)
	ctx := testContext("req-bg")
	term := workspace.NewTerminalHandler(ws, pm)
	manage := NewProcessManageHandler(ws, pm)
	res, err := term(ctx, map[string]interface{}{"command": "echo bg-hi", "background": true, "timeout": float64(30)})
	if err != nil || !strings.Contains(res.Content, "session_id:") {
		t.Fatalf("background spawn = %+v %v", res, err)
	}
	waited, err := manage(ctx, map[string]interface{}{"action": "wait", "session_id": sessionFrom(res.Content), "timeout": float64(10)})
	if err != nil || !strings.Contains(waited.Content, "bg-hi") {
		t.Fatalf("wait = %+v %v", waited, err)
	}
}
