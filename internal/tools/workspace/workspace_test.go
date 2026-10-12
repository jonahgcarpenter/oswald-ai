package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/process"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func testContext(requestID string) context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID})
	return ctx
}

func TestResolveRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	for _, raw := range []string{"../x", "../../etc/passwd", "/etc/passwd", "~/../x"} {
		if _, _, err := ws.Resolve(raw); err == nil {
			t.Fatalf("escape allowed: %q", raw)
		}
	}
	abs, rel, err := ws.Resolve("sub/dir/file.txt")
	if err != nil || rel != filepath.Join("sub", "dir", "file.txt") || !strings.HasPrefix(abs, root) {
		t.Fatalf("resolve = %q %q %v", abs, rel, err)
	}
	if _, _, err := ws.Resolve("~/notes.md"); err != nil {
		t.Fatalf("tilde resolve failed: %v", err)
	}
}

func TestDefaultDeniesProfilesAndSecrets(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "default")
	for _, raw := range []string{"profiles/alice/x", "config.yaml", ".env", "state.db", "state.db-wal", "mcp-tokens/tok", "memories/MEMORY.md", "SOUL.md"} {
		if _, _, err := ws.Resolve(raw); err == nil {
			t.Fatalf("denied path allowed: %q", raw)
		}
	}
	namedRoot := filepath.Join(root, "profiles", "alice")
	if err := os.MkdirAll(namedRoot, 0700); err != nil {
		t.Fatal(err)
	}
	named := NewWorkspace(namedRoot, "alice")
	for _, raw := range []string{"config.yaml", "state.db", "mcp-tokens/x"} {
		if _, _, err := named.Resolve(raw); err == nil {
			t.Fatalf("named denied path allowed: %q", raw)
		}
	}
	if _, _, err := named.Resolve("notes.md"); err != nil {
		t.Fatalf("named allowed path rejected: %v", err)
	}
}

func TestReadWriteGate(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	ctx := testContext("req-1")
	write := NewWriteFileHandler(ws)
	read := NewReadFileHandler(ws)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := write(ctx, map[string]interface{}{"path": "a.txt", "content": "bye"}); err == nil {
		t.Fatal("write without read succeeded")
	}
	if _, err := read(ctx, map[string]interface{}{"path": "a.txt"}); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	res, err := write(ctx, map[string]interface{}{"path": "a.txt", "content": "bye"})
	if err != nil || !strings.Contains(res.Content, "verified") {
		t.Fatalf("write after read = %+v %v", res, err)
	}
	// External modification invalidates the gate.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := write(ctx, map[string]interface{}{"path": "a.txt", "content": "again"}); err == nil {
		t.Fatal("stale write succeeded after external change")
	}
}

func TestPatchUniqueAndReplaceAll(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
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

func TestSearchFilesContentAndNames(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
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

func TestTerminalForeground(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	ctx := testContext("req-t")
	term := NewTerminalHandler(ws, process.NewManager(root))
	res, err := term(ctx, map[string]interface{}{"command": "echo hi", "timeout": float64(10)})
	if err != nil || !strings.Contains(res.Content, "exit_code: 0") || !strings.Contains(res.Content, "hi") {
		t.Fatalf("terminal = %+v %v", res, err)
	}
	if _, err := term(ctx, map[string]interface{}{"command": "echo hi", "background": true}); err != nil {
		// Background spawns are valid; poll them to completion.
		t.Fatalf("background spawn failed: %v", err)
	}
	if _, err := term(ctx, map[string]interface{}{"command": "echo hi", "workdir": "../"}); err == nil {
		t.Fatal("escaping workdir accepted")
	}
}

func TestTerminalBackgroundRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	pm := process.NewManager(root)
	ctx := testContext("req-bg")
	term := NewTerminalHandler(ws, pm)
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

func sessionFrom(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "session_id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
		}
	}
	return ""
}

func TestExecuteCodeStateless(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	ctx := testContext("req-e")
	exec := NewExecuteCodeHandler(ws)
	res, err := exec(ctx, map[string]interface{}{"code": "print(40+2)"})
	if err != nil || !strings.Contains(res.Content, "42") {
		t.Fatalf("execute_code = %+v %v (python3 may be missing)", res, err)
	}
}

func TestDefinitionsContract(t *testing.T) {
	defs := Definitions()
	if len(defs) != 7 {
		t.Fatalf("definitions = %d, want 7", len(defs))
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if d.Name == "" || d.Description == "" || d.Parameters.Type != "object" || d.Parameters.Properties == nil {
			t.Fatalf("invalid definition: %+v", d)
		}
		seen[d.Name] = true
	}
	for _, name := range []string{ReadFileName, WriteFileName, PatchName, SearchFilesName, TerminalName, ExecuteCodeName, ProcessManageName} {
		if !seen[name] {
			t.Fatalf("missing definition %s", name)
		}
	}
}

func TestDynamicDescriptionOCROverride(t *testing.T) {
	SetOCRAvailable(false)
	base := ReadFileDefinition().Description
	if got := DynamicDescription(ReadFileName, base); got != base {
		t.Fatal("override applied without OCR")
	}
	SetOCRAvailable(true)
	defer SetOCRAvailable(false)
	got := DynamicDescription(ReadFileName, base)
	if !strings.Contains(got, "PDF (scanned or text)") || strings.Contains(got, "PDF (text layer)") {
		t.Fatalf("ocr override = %q", got)
	}
}
