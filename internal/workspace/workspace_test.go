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
	if err := os.WriteFile(filepath.Join(root, "job.sh"), []byte("#!/bin/sh\necho cron-hi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := testContext("req-cron")
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "script": "job.sh", "no_agent": true})
	if err != nil || !strings.Contains(created.Content, "job_id:") {
		t.Fatalf("create = %+v %v", created, err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
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
}

func TestCronjobManageNameAndOrigin(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-name"), requestctx.Metadata{RequestID: "req-cron-name", SessionID: "discord:chan1:alice"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "name": "brief", "schedule": "in 1h", "prompt": "hi"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "brief" || stored.OriginGateway != "discord" || stored.OriginSession != "discord:chan1:alice" {
		t.Fatalf("origin not captured: %+v", stored)
	}
	if stored.Deliver != "origin" {
		t.Fatalf("deliver not defaulted to origin: %+v", stored)
	}
	// Fuzzy name resolution on update.
	updated, err := manage(ctx, map[string]interface{}{"action": "pause", "job_id": "brie", "paused_reason": "test"})
	if err != nil || !strings.Contains(updated.Content, "paused") {
		t.Fatalf("fuzzy pause = %+v %v", updated, err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "name": "brief-two", "schedule": "in 1h", "prompt": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "remove", "job_id": "rief"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous remove = %v", err)
	}
}

func TestCronjobManageDeliverOriginOnly(t *testing.T) {
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
	for _, target := range []string{"local", "all", "discord:chan1:user1"} {
		if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver": target}); err == nil {
			t.Fatalf("target %q accepted", target)
		}
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver": ""}); err != nil {
		t.Fatalf("empty reset = %v", err)
	}
	stored, err := store.Resolve(jobID)
	if err != nil || stored.Deliver != "origin" {
		t.Fatalf("reset = %+v %v", stored, err)
	}
	if _, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi", "failure_deliver": "origin"}); err == nil {
		t.Fatal("failure_deliver accepted")
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
	notified := make(chan string, 2)
	ctx = WithEscalationNotify(ctx, func(text string) {
		notified <- text
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
		if !strings.Contains(n, "/approve") {
			t.Fatalf("first notice is not the ask: %q", n)
		}
		select {
		case n := <-notified:
			if !strings.Contains(n, "expired") || !strings.Contains(n, "denied") {
				t.Fatalf("expiry follow-up wrong: %q", n)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no expiry follow-up sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no escalation notification sent")
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
	notified := make(chan string, 2)
	ctx = WithEscalationNotify(ctx, func(text string) {
		notified <- text
	})
	done := make(chan struct{}, 1)
	go func() {
		_, _ = term(ctx, map[string]interface{}{"command": "sudo id", "timeout": float64(30)})
		done <- struct{}{}
	}()
	select {
	case n := <-notified:
		if !strings.Contains(n, "sudo id") || !strings.Contains(n, "/approve") {
			t.Fatalf("notification = %q", n)
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
	cases := []struct {
		call string
		args map[string]interface{}
		want string
	}{
		{"read", map[string]interface{}{"path": ".env.local"}, "secret-bearing environment file"},
		{"read", map[string]interface{}{"path": "state.db"}, "application-owned state"},
		{"read", map[string]interface{}{"path": "config.yaml"}, "operator configuration"},
		{"read", map[string]interface{}{"path": "profiles/alice/x"}, "another profile"},
		{"read", map[string]interface{}{"path": "memories/MEMORY.md"}, "managed memory state"},
		{"write", map[string]interface{}{"path": "SOUL.md", "content": "x"}, "protected operator policy"},
		{"write", map[string]interface{}{"path": "memories/MEMORY.md", "content": "x"}, "Memory write denied"},
		{"write", map[string]interface{}{"path": "deploy.pem", "content": "x"}, "secret-bearing"},
	}
	for _, tc := range cases {
		var err error
		if tc.call == "read" {
			_, err = read(testContext("req-deny"), tc.args)
		} else {
			_, err = write(testContext("req-deny"), tc.args)
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
