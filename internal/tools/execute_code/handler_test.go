package execcode

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

func profilePair(t *testing.T) (tree, alice, bob string) {
	t.Helper()
	tree = filepath.Join(t.TempDir(), ".oswald")
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
	if err := os.WriteFile(filepath.Join(tree, "config.yaml"), []byte("x: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	return tree, alice, bob
}

func TestDefinitionContract(t *testing.T) {
	def := Definition()
	if def.Name != Name || def.Name == "" || def.Description == "" {
		t.Fatalf("invalid definition: %+v", def)
	}
	if def.Parameters.Type != "object" || def.Parameters.Properties == nil {
		t.Fatalf("invalid parameters: %+v", def)
	}
}

func TestExecuteCodeStateless(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	ctx := testContext("req-e")
	exec := NewExecuteCodeHandler(ws)
	res, err := exec(ctx, map[string]interface{}{"code": "print(40+2)"})
	if err != nil || !strings.Contains(res.Content, "42") {
		t.Fatalf("execute_code = %+v %v (python3 may be missing)", res, err)
	}
}

func TestExecuteCodeConfinedWrite(t *testing.T) {
	_, alice, bob := profilePair(t)
	ws := workspace.NewWorkspace(alice, "alice")
	exec := NewExecuteCodeHandler(ws)
	res, err := exec(testContext("req-xcode"), map[string]interface{}{
		"code": "print(open('" + filepath.Join(bob, "SOUL.md") + "').read())",
	})
	if err != nil || !strings.Contains(res.Content, "bob soul") {
		t.Fatalf("cross-profile code read failed: %+v %v", res, err)
	}
	_, err = exec(testContext("req-xcode-w"), map[string]interface{}{
		"code": "open('" + filepath.Join(bob, "notes.md") + "', 'w').write('x')",
	})
	if err == nil || !strings.Contains(err.Error(), "system-managed") {
		t.Fatalf("code allowed cross-profile write: %v", err)
	}
}

func TestExecuteCodeSetsOswaldHome(t *testing.T) {
	_, alice, _ := profilePair(t)
	ws := workspace.NewWorkspace(alice, "alice")
	exec := NewExecuteCodeHandler(ws)
	res, err := exec(testContext("req-home"), map[string]interface{}{
		"code": "import os; print(os.environ.get('OSWALD_HOME', ''))",
	})
	if err != nil {
		t.Fatalf("execute_code failed: %v", err)
	}
	if !strings.Contains(res.Content, alice) {
		t.Fatalf("OSWALD_HOME missing profile root: %q", res.Content)
	}
}

func TestExecuteCodeHomeAndUserInstallEnv(t *testing.T) {
	_, alice, _ := profilePair(t)
	ws := workspace.NewWorkspace(alice, "alice")
	exec := NewExecuteCodeHandler(ws)
	res, err := exec(testContext("req-home2"), map[string]interface{}{
		"code": "import os; print(os.environ.get('HOME', '')); print(os.environ.get('npm_config_prefix', '')); print(os.environ.get('XDG_CACHE_HOME', 'absent')); print(os.environ.get('OSWALD_WORKSPACE', 'absent'))",
	})
	if err != nil {
		t.Fatalf("execute_code failed: %v", err)
	}
	if !strings.Contains(res.Content, alice+"\n") {
		t.Fatalf("HOME missing profile root: %q", res.Content)
	}
	if !strings.Contains(res.Content, filepath.Join(alice, ".local")+"\n") {
		t.Fatalf("npm_config_prefix missing: %q", res.Content)
	}
	if strings.Count(res.Content, "absent") != 2 {
		t.Fatalf("legacy vars still set: %q", res.Content)
	}
}

func TestExecuteCodeDeniesManagedCache(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	exec := NewExecuteCodeHandler(ws)
	_, err := exec(testContext("req-ccode"), map[string]interface{}{
		"code": `open('cache/foo', 'w')`,
	})
	if err == nil || !strings.Contains(err.Error(), "cache/scratch") {
		t.Fatalf("code allowed managed cache write: %v", err)
	}
}

func TestExecuteCodeDeniesScopeOverride(t *testing.T) {
	_, alice, _ := profilePair(t)
	ws := workspace.NewWorkspace(alice, "alice")
	exec := NewExecuteCodeHandler(ws)
	_, err := exec(testContext("req-scope"), map[string]interface{}{
		"code": `import os; os.environ["OSWALD_HOME"] = "/tmp/x"`,
	})
	if err == nil || !strings.Contains(err.Error(), "OSWALD_*") {
		t.Fatalf("code allowed scope override: %v", err)
	}
}
