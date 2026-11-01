package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/approvals"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/cron"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace/process"
)

func testContext(requestID string) context.Context {
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "alice", Assurance: identity.AssuranceDiscordGateway})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: requestID})
	return ctx
}

func TestResolveReadsOutside(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	// Outside reads resolve (system-managed); only protected names fail.
	for _, raw := range []string{"../x", "../../etc/passwd", "/etc/hostname", "/proc/cpuinfo", "~/../x"} {
		abs, display, err := ws.Resolve(raw)
		if err != nil {
			t.Fatalf("outside read denied: %q: %v", raw, err)
		}
		if filepath.IsAbs(raw) && (abs != raw || display != raw) {
			t.Fatalf("outside resolve = %q %q", abs, display)
		}
		if _, _, err := ws.ResolveWrite(raw); err == nil {
			t.Fatalf("outside write allowed: %q", raw)
		}
	}
	for _, raw := range []string{"/proc/self/environ", "/proc/1/cmdline", "../.env"} {
		if _, _, err := ws.Resolve(raw); err == nil {
			t.Fatalf("protected outside read allowed: %q", raw)
		}
	}
	for _, raw := range []string{"", "   ", "bad\x00path", strings.Repeat("x", 5000)} {
		if _, _, err := ws.Resolve(raw); err == nil {
			t.Fatalf("invalid path allowed: %q", raw)
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

func TestSameAndCrossProfileGroups(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "default")
	// Same-profile: secrets and operator config sealed, everything else readable.
	for _, raw := range []string{".env", ".env.local", "config.yaml", "mcp/robinhood/robinhood.json", "mcp/spotify/auth.json", "deploy.pem", "vault/credentials.json"} {
		if _, _, err := ws.Resolve(raw); err == nil {
			t.Fatalf("protected path allowed: %q", raw)
		}
	}
	for _, raw := range []string{"state.db", "SOUL.md", "memories/MEMORY.md", "cache/images/a.png", "cache/scratch/a.txt", "notes.md"} {
		if _, _, err := ws.Resolve(raw); err != nil {
			t.Fatalf("read denied: %q: %v", raw, err)
		}
	}
	if _, _, err := ws.ResolveWrite("SOUL.md"); err != nil {
		t.Fatalf("same-profile SOUL.md write denied: %v", err)
	}
	if _, _, err := ws.ResolveWrite("notes.md"); err != nil {
		t.Fatalf("same-profile notes write denied: %v", err)
	}
	for _, raw := range []string{"config.yaml", "state.db", "memories/MEMORY.md", "cache/images/a.png", ".env"} {
		if _, _, err := ws.ResolveWrite(raw); err == nil {
			t.Fatalf("protected write allowed: %q", raw)
		}
	}
	// Cross-profile: readable except secrets, operator config, and state.db.
	for _, raw := range []string{"profiles/alice/SOUL.md", "profiles/alice/notes.md", "profiles/alice/memories/USER.md", "profiles/alice/cache/scratch/a.txt"} {
		if _, _, err := ws.Resolve(raw); err != nil {
			t.Fatalf("cross-profile read denied: %q: %v", raw, err)
		}
		if _, _, err := ws.ResolveWrite(raw); err == nil {
			t.Fatalf("cross-profile write allowed: %q", raw)
		}
	}
	for _, raw := range []string{"profiles/alice/.env", "profiles/alice/config.yaml", "profiles/alice/state.db"} {
		if _, _, err := ws.Resolve(raw); err == nil {
			t.Fatalf("cross-profile secret allowed: %q", raw)
		}
	}
	namedRoot := filepath.Join(root, "profiles", "alice")
	if err := os.MkdirAll(namedRoot, 0700); err != nil {
		t.Fatal(err)
	}
	named := NewWorkspace(namedRoot, "alice")
	for _, raw := range []string{"SOUL.md", "state.db", "notes.md"} {
		if _, _, err := named.Resolve(raw); err != nil {
			t.Fatalf("named read denied: %q: %v", raw, err)
		}
	}
	if _, _, err := named.Resolve("mcp/spotify/auth.json"); err == nil {
		t.Fatal("named secret allowed")
	}
	if _, _, err := named.ResolveWrite("SOUL.md"); err != nil {
		t.Fatalf("named SOUL.md write denied: %v", err)
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
	pm := process.NewManager(root)
	ctx := testContext("req-t")
	term := NewTerminalHandler(ws, pm)
	res, err := term(ctx, map[string]interface{}{"command": "echo hi", "timeout": float64(10)})
	if err != nil || !strings.Contains(res.Content, "exit_code: 0") || !strings.Contains(res.Content, "hi") {
		t.Fatalf("terminal = %+v %v", res, err)
	}
	spawned, err := term(ctx, map[string]interface{}{"command": "echo hi", "background": true})
	if err != nil {
		// Background spawns are valid; poll them to completion.
		t.Fatalf("background spawn failed: %v", err)
	}
	// Join the background spawn so its watcher finishes all disk writes
	// before TempDir cleanup; otherwise removal races completion metadata.
	if _, err := pm.Wait(sessionFrom(spawned.Content), 10); err != nil {
		t.Fatalf("background wait failed: %v", err)
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
	if len(defs) != 8 {
		t.Fatalf("definitions = %d, want 8", len(defs))
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if d.Name == "" || d.Description == "" || d.Parameters.Type != "object" || d.Parameters.Properties == nil {
			t.Fatalf("invalid definition: %+v", d)
		}
		seen[d.Name] = true
	}
	for _, name := range []string{ReadFileName, WriteFileName, PatchName, SearchFilesName, TerminalName, ExecuteCodeName, ProcessManageName, CronjobManageName} {
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

func TestCronjobManageCreateListRun(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "job.sh"), []byte("#!/bin/sh\necho cron-hi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron"), requestctx.Metadata{RequestID: "req-cron", SessionID: "discord:chan1:alice"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "script": "job.sh", "no_agent": true})
	if err != nil || !strings.Contains(created.Content, "job_id:") {
		t.Fatalf("create = %+v %v", created, err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Deliver.Platform != "discord" || stored.Deliver.ChatID != "chan1" || stored.Deliver.UserID != "alice" {
		t.Fatalf("deliver not defaulted to creating chat: %+v", stored.Deliver)
	}
	listed, err := manage(ctx, map[string]interface{}{"action": "list"})
	if err != nil || !strings.Contains(listed.Content, jobID) {
		t.Fatalf("list = %+v %v", listed, err)
	}
	run, err := manage(ctx, map[string]interface{}{"action": "run", "job_id": jobID})
	if err != nil || !strings.Contains(run.Content, "cron-hi") {
		t.Fatalf("run = %+v %v", run, err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "script": "job.sh", "model": "x"}); err == nil {
		t.Fatal("model override accepted")
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi", "monitor_url": "https://x.example"}); err == nil {
		t.Fatal("dropped monitor field accepted")
	}
}

func TestCronjobManageNameAndDeliver(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-name"), requestctx.Metadata{RequestID: "req-cron-name", SessionID: "discord:chan1:alice"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "name": "brief", "schedule": "in 1h", "prompt": "hi", "deliver_platform": "discord", "deliver_chat_id": "chan9", "deliver_user_id": "bob"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "brief" || stored.Deliver.Platform != "discord" || stored.Deliver.ChatID != "chan9" || stored.Deliver.UserID != "bob" {
		t.Fatalf("deliver not stored: %+v", stored)
	}
	// Fuzzy name resolution on update.
	updated, err := manage(ctx, map[string]interface{}{"action": "pause", "job_id": "brie", "paused_reason": "test"})
	if err != nil || !strings.Contains(updated.Content, "paused") {
		t.Fatalf("fuzzy pause = %+v %v", updated, err)
	}
	stored, err = store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != cron.StatePaused || stored.NextRunAt != nil || stored.PausedReason != "test" {
		t.Fatalf("pause = %+v", stored)
	}
	resumed, err := manage(ctx, map[string]interface{}{"action": "resume", "job_id": jobID})
	if err != nil || !strings.Contains(resumed.Content, "resumed") {
		t.Fatalf("resume = %+v %v", resumed, err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "name": "brief-two", "schedule": "in 1h", "prompt": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "remove", "job_id": "rief"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous remove = %v", err)
	}
}

func TestCronjobManageDeliverValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-deliver"), requestctx.Metadata{RequestID: "req-cron-deliver", SessionID: "discord:chan1:alice"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_platform": "sms"}); err == nil {
		t.Fatal("bad platform accepted")
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_chat_id": ""}); err == nil {
		t.Fatal("empty chat accepted")
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_chat_id": "chan2"}); err != nil {
		t.Fatalf("chat update = %v", err)
	}
	stored, err := store.Resolve(jobID)
	if err != nil || stored.Deliver.ChatID != "chan2" || stored.Deliver.Platform != "discord" {
		t.Fatalf("deliver merge = %+v %v", stored, err)
	}
	// No session context and no explicit target fails closed.
	bare := testContext("req-cron-bare")
	if _, err := manage(bare, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi"}); err == nil {
		t.Fatal("missing deliver accepted without session")
	}
}

func TestTerminalDangerousApproveExecutes(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "wiped")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-review-approve"), StubReviewer{Verdict: VerdictApprove})
	res, err := term(ctx, map[string]interface{}{"command": "chmod -R 777 " + target, "timeout": float64(30)})
	if err != nil || !strings.Contains(res.Content, "exit_code: 0") {
		t.Fatalf("approved dangerous = %+v %v", res, err)
	}
}

func TestTerminalDangerousDenyBlocks(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-review-deny"), StubReviewer{Verdict: VerdictDeny})
	if _, err := term(ctx, map[string]interface{}{"command": "sudo apt update", "timeout": float64(30)}); err == nil || !strings.Contains(err.Error(), "denied by security review") {
		t.Fatalf("deny = %v", err)
	}
}

func TestTerminalDangerousEscalateBlocksUntilApproved(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "wiped")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-review-escalate"), StubReviewer{Verdict: VerdictEscalate})
	type toolResult struct {
		content string
		err     error
	}
	results := make(chan toolResult, 1)
	go func() {
		res, err := term(ctx, map[string]interface{}{"command": "chmod -R 777 " + target, "timeout": float64(30)})
		results <- toolResult{content: res.Content, err: err}
	}()
	// The tool call must not return while the ask is pending.
	select {
	case r := <-results:
		t.Fatalf("escalated call returned early: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	if !approvals.Has("alice", "req-review-escalate") {
		t.Fatal("no pending ask while blocked")
	}
	// Simulate /approve: take and signal, then the waiter executes.
	stored, ok, expired := approvals.Take("alice", "req-review-escalate")
	if !ok || expired {
		t.Fatalf("pending = %+v ok=%t expired=%t", stored, ok, expired)
	}
	approvals.Notify(stored, approvals.DecisionApproved)
	select {
	case r := <-results:
		if r.err != nil || !strings.Contains(r.content, "exit_code: 0") {
			t.Fatalf("approved result = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approved waiter never returned")
	}
	if approvals.Has("alice", "req-review-escalate") {
		t.Fatal("pending ask survived approval")
	}
}

func TestTerminalDangerousDeniedReturnsToolResult(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-review-deny-result"), StubReviewer{Verdict: VerdictEscalate})
	type toolResult struct {
		content string
		err     error
	}
	results := make(chan toolResult, 1)
	go func() {
		res, err := term(ctx, map[string]interface{}{"command": "sudo id", "timeout": float64(30)})
		results <- toolResult{content: res.Content, err: err}
	}()
	select {
	case r := <-results:
		t.Fatalf("escalated call returned early: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	stored, ok, expired := approvals.Take("alice", "req-review-deny-result")
	if !ok || expired {
		t.Fatalf("pending = %+v ok=%t expired=%t", stored, ok, expired)
	}
	approvals.Notify(stored, approvals.DecisionDenied)
	select {
	case r := <-results:
		if r.err != nil {
			t.Fatalf("denial errored: %+v", r)
		}
		if !strings.Contains(r.content, "denied") {
			t.Fatalf("denial missing: %q", r.content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("denied waiter never returned")
	}
}

func TestTerminalDangerousExpiryDenies(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	old := approvals.ApprovalWindow
	approvals.ApprovalWindow = 50 * time.Millisecond
	defer func() { approvals.ApprovalWindow = old }()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-review-expiry"), StubReviewer{Verdict: VerdictEscalate})
	notified := make(chan EscalationNotice, 2)
	ctx = WithEscalationNotify(ctx, func(n EscalationNotice) {
		notified <- n
	})
	res, err := term(ctx, map[string]interface{}{"command": "sudo id", "timeout": float64(30)})
	if err != nil {
		t.Fatalf("expiry errored: %v", err)
	}
	if !strings.Contains(res.Content, "denied") && !strings.Contains(res.Content, "expired") {
		t.Fatalf("expiry result wrong: %q", res.Content)
	}
	select {
	case n := <-notified:
		// First notice is the ask; the second must be the expiry follow-up.
		if n.Expired || n.Command != "sudo id" {
			t.Fatalf("first notice is not the ask: %+v", n)
		}
		select {
		case n := <-notified:
			if !n.Expired {
				t.Fatalf("expiry follow-up missing flag: %+v", n)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no expiry follow-up sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no escalation notification sent")
	}
}

func TestEscalationDiscordTextFencesCommand(t *testing.T) {
	got := EscalationStatusDiscordText("decode-pipe to shell", "cat cache/scratch/payload.b64 | base64 -d | sh")
	if !strings.Contains(got, "**Security review") || !strings.Contains(got, "```bash\ncat cache/scratch/payload.b64 | base64 -d | sh\n```") || !strings.Contains(got, "`/approve`") || !strings.Contains(got, "`/deny`") {
		t.Fatalf("discord ask wrong:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("ask has runaway blank lines:\n%s", got)
	}
	got = EscalationStatusDiscordText("x", "echo `hi`")
	if strings.Contains(got, "`hi`") || !strings.Contains(got, "'hi'") {
		t.Fatalf("backticks not neutralized:\n%s", got)
	}
	long := strings.Repeat("y", 2000)
	if runes := []rune(EscalationStatusDiscordText("x", long)); len(runes) > 1700 {
		t.Fatalf("ask not bounded: %d runes", len(runes))
	}
	if got := EscalationExpiredDiscordText("decode-pipe to shell"); !strings.Contains(got, "expired") || !strings.Contains(got, "denied") {
		t.Fatalf("discord expiry wrong: %q", got)
	}
}

func TestTerminalDangerousUnattendedDeniesImmediately(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "cron", ExternalID: "cron:alice", Assurance: identity.AssuranceCronScheduler})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req-review-cron"})
	ctx = WithReviewer(ctx, StubReviewer{Verdict: VerdictEscalate})
	if _, err := term(ctx, map[string]interface{}{"command": "sudo id", "timeout": float64(30)}); err == nil || !strings.Contains(err.Error(), "unavailable on this transport") {
		t.Fatalf("cron escalate = %v", err)
	}
	if approvals.Has("alice", "req-review-cron") {
		t.Fatal("unattended transport parked an ask")
	}
}

func TestTerminalDangerousEscalationNotifiesWhileBlocked(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-review-notify"), StubReviewer{Verdict: VerdictEscalate})
	notified := make(chan EscalationNotice, 2)
	ctx = WithEscalationNotify(ctx, func(n EscalationNotice) {
		notified <- n
	})
	done := make(chan struct{}, 1)
	go func() {
		_, _ = term(ctx, map[string]interface{}{"command": "sudo id", "timeout": float64(30)})
		done <- struct{}{}
	}()
	select {
	case n := <-notified:
		if n.Expired || n.Command != "sudo id" || n.Reason == "" {
			t.Fatalf("notification = %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no escalation notification while blocked")
	}
	approvals.DenySession("alice", "req-review-notify")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter leaked after DenySession")
	}
}

func TestTerminalDangerousSingleFlight(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-review-flight"), StubReviewer{Verdict: VerdictEscalate})
	type toolResult struct {
		content string
		err     error
	}
	first := make(chan toolResult, 1)
	go func() {
		res, err := term(ctx, map[string]interface{}{"command": "sudo id", "timeout": float64(30)})
		first <- toolResult{content: res.Content, err: err}
	}()
	select {
	case r := <-first:
		t.Fatalf("first escalate returned early: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := term(ctx, map[string]interface{}{"command": "sudo whoami", "timeout": float64(30)}); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("second escalate = %v", err)
	}
	if !approvals.DenySession("alice", "req-review-flight") {
		t.Fatal("DenySession found nothing")
	}
	select {
	case r := <-first:
		if r.err != nil || !strings.Contains(r.content, "denied") {
			t.Fatalf("denied result = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("denied waiter never returned")
	}
}

func TestTerminalDangerousWithoutReviewerFailsClosed(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	if _, err := term(testContext("req-review-none"), map[string]interface{}{"command": "sudo id", "timeout": float64(30)}); err == nil || !strings.Contains(err.Error(), "review is unavailable") {
		t.Fatalf("no reviewer = %v", err)
	}
}

func TestDenyMessagesCategorized(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "default")
	read := NewReadFileHandler(ws)
	write := NewWriteFileHandler(ws)
	for _, p := range []string{"config.yaml", "SOUL.md", "state.db", filepath.Join("memories", "MEMORY.md"), filepath.Join("profiles", "alice", "x"), filepath.Join("skills", "review", "SKILL.md")} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		call string
		args map[string]interface{}
		want string
	}{
		{"read", map[string]interface{}{"path": ".env.local"}, "protected"},
		{"read", map[string]interface{}{"path": "profiles/alice/state.db"}, "session_search"},
		{"read", map[string]interface{}{"path": "config.yaml"}, "oswald config"},
		{"read", map[string]interface{}{"path": "profiles/alice/x"}, ""},
		{"read", map[string]interface{}{"path": "memories/MEMORY.md"}, ""},
		{"read", map[string]interface{}{"path": "skills/review/SKILL.md"}, ""},
		{"read", map[string]interface{}{"path": "SOUL.md"}, ""},
		{"write", map[string]interface{}{"path": "SOUL.md", "content": "x"}, ""},
		{"write", map[string]interface{}{"path": "config.yaml", "content": "x"}, "oswald providers"},
		{"write", map[string]interface{}{"path": "memories/MEMORY.md", "content": "x"}, "memory tool"},
		{"write", map[string]interface{}{"path": "skills/review/SKILL.md", "content": "x"}, "skill_manage tool"},
		{"write", map[string]interface{}{"path": "deploy.pem", "content": "x"}, "protected"},
		{"write", map[string]interface{}{"path": "profiles/alice/notes.md", "content": "x"}, "system-managed"},
	}
	for _, tc := range cases {
		var err error
		if tc.call == "read" {
			_, err = read(testContext("req-deny"), tc.args)
		} else {
			// Seed the read-before-write gate for agent-owned paths.
			if tc.want == "" {
				if _, rerr := read(testContext("req-deny"), map[string]interface{}{"path": tc.args["path"]}); rerr != nil {
					t.Fatalf("seed read %v = %v", tc.args["path"], rerr)
				}
			}
			_, err = write(testContext("req-deny"), tc.args)
		}
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s %v = %v, want success", tc.call, tc.args["path"], err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s %v = %v, want %q", tc.call, tc.args["path"], err, tc.want)
		}
	}
}

func TestTerminalUnrecoverableEnvelope(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := WithReviewer(testContext("req-unrec"), StubReviewer{Verdict: VerdictApprove})
	res, err := term(ctx, map[string]interface{}{"command": "rm -rf /", "timeout": float64(30)})
	if err != nil {
		t.Fatalf("unrecoverable errored: %v", err)
	}
	for _, want := range []string{`"status":"blocked"`, `"exit_code":1`, "Do NOT retry"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("envelope missing %q: %s", want, res.Content)
		}
	}
}

func TestExecuteApprovedRefusesUnrecoverable(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	pending := approvals.Pending{Profile: "alice", SessionKey: "s", Command: "rm -rf /", Workdir: root, Scratch: root, Timeout: 30}
	if _, err := ExecuteApproved(context.Background(), ws, process.NewManager(root), pending); err == nil || !strings.Contains(err.Error(), "refused on re-scan") {
		t.Fatalf("replay allowed unrecoverable: %v", err)
	}
}
