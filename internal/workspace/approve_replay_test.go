package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/approvals"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

func profileTreeForApprove(t *testing.T) (tree, alice, bob string) {
	t.Helper()
	tree = filepath.Join(t.TempDir(), ".oswald")
	alice = filepath.Join(tree, "profiles", "alice")
	bob = filepath.Join(tree, "profiles", "bob")
	for _, dir := range []string{alice, bob} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return tree, alice, bob
}

func TestExecuteApprovedRefusesProtectedRead(t *testing.T) {
	_, alice, bob := profileTreeForApprove(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(alice, "alice")
	pending := approvals.Pending{Profile: "alice", SessionKey: "s", Command: "cat " + filepath.Join(bob, "state.db"), Workdir: alice, Scratch: alice, Timeout: 30}
	if _, err := ExecuteApproved(context.Background(), ws, process.NewManager(alice), pending); err == nil || !strings.Contains(err.Error(), "refused on re-scan") {
		t.Fatalf("replay allowed protected read: %v", err)
	}
}
