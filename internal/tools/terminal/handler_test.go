package toolterminal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/approvals"
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

func profileTree(t *testing.T) (tree, alice, bob string) {
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

func sessionFrom(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "session_id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
		}
	}
	return ""
}

func TestTerminalForeground(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-review-approve"), workspace.StubReviewer{Verdict: workspace.VerdictApprove})
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-review-deny"), workspace.StubReviewer{Verdict: workspace.VerdictDeny})
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-review-escalate"), workspace.StubReviewer{Verdict: workspace.VerdictEscalate})
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-review-deny-result"), workspace.StubReviewer{Verdict: workspace.VerdictEscalate})
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-review-expiry"), workspace.StubReviewer{Verdict: workspace.VerdictEscalate})
	notified := make(chan workspace.EscalationNotice, 2)
	ctx = workspace.WithEscalationNotify(ctx, func(n workspace.EscalationNotice) {
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

func TestTerminalDangerousUnattendedDeniesImmediately(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := context.Background()
	ctx = requestctx.WithPrincipal(ctx, identity.Principal{CanonicalUserID: "alice", Gateway: "cron", ExternalID: "cron:alice", Assurance: identity.AssuranceCronScheduler})
	ctx = requestctx.WithMetadata(ctx, requestctx.Metadata{RequestID: "req-review-cron"})
	ctx = workspace.WithReviewer(ctx, workspace.StubReviewer{Verdict: workspace.VerdictEscalate})
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-review-notify"), workspace.StubReviewer{Verdict: workspace.VerdictEscalate})
	notified := make(chan workspace.EscalationNotice, 2)
	ctx = workspace.WithEscalationNotify(ctx, func(n workspace.EscalationNotice) {
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-review-flight"), workspace.StubReviewer{Verdict: workspace.VerdictEscalate})
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
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	if _, err := term(testContext("req-review-none"), map[string]interface{}{"command": "sudo id", "timeout": float64(30)}); err == nil || !strings.Contains(err.Error(), "review is unavailable") {
		t.Fatalf("no reviewer = %v", err)
	}
}

func TestTerminalUnrecoverableEnvelope(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	ctx := workspace.WithReviewer(testContext("req-unrec"), workspace.StubReviewer{Verdict: workspace.VerdictApprove})
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

func TestTerminalSetsOswaldHome(t *testing.T) {
	_, alice, _ := profileTree(t)
	ws := workspace.NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, nil)
	res, err := term(testContext("req-home"), map[string]interface{}{
		"command": "printf %s \"$OSWALD_HOME\"",
	})
	if err != nil {
		t.Fatalf("terminal failed: %v", err)
	}
	if !strings.Contains(res.Content, alice) {
		t.Fatalf("OSWALD_HOME missing profile root: %q", res.Content)
	}
}

func TestTerminalPassthroughResolvesProfileScope(t *testing.T) {
	tree, alice, _ := profileTree(t)
	if err := os.WriteFile(filepath.Join(tree, ".env"), []byte("SYNTH_SHARED_VAR=synthetic-shared\nSYNTH_LAUNCH_ONLY=synthetic-launch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alice, ".env"), []byte("SYNTH_PROFILE_VAR=synthetic-profile\n"), 0600); err != nil {
		t.Fatal(err)
	}
	process.ClearEnvPassthrough()
	defer process.ClearEnvPassthrough()
	if refused := process.RegisterEnvPassthrough([]string{"SYNTH_SHARED_VAR", "SYNTH_PROFILE_VAR", "OPENAI_API_KEY"}); len(refused) != 1 {
		t.Fatalf("refused = %v", refused)
	}
	ws := workspace.NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, nil)
	res, err := term(testContext("req-passthrough"), map[string]interface{}{
		"command": "printf '%s|%s|%s' \"$SYNTH_SHARED_VAR\" \"$SYNTH_PROFILE_VAR\" \"$SYNTH_LAUNCH_ONLY\"",
	})
	if err != nil {
		t.Fatalf("terminal failed: %v", err)
	}
	if !strings.Contains(res.Content, "synthetic-shared|synthetic-profile|") {
		t.Fatalf("passthrough values missing: %q", res.Content)
	}
	if strings.Contains(res.Content, "synthetic-launch") {
		t.Fatalf("unregistered name leaked: %q", res.Content)
	}
}

func TestTerminalDeniesManagedCache(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(root, "alice")
	term := NewTerminalHandler(ws, process.NewManager(root))
	res, err := term(testContext("req-cache"), map[string]interface{}{
		"command": "mkdir cache/scratch_go",
	})
	if err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	if !strings.Contains(res.Content, `"status":"blocked"`) || !strings.Contains(res.Content, "cache/scratch") {
		t.Fatalf("missing cache blocked envelope: %q", res.Content)
	}
}

func TestTerminalConfinedWriteEnvelope(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, process.NewManager(alice))
	// Reads roam, including cross-profile.
	res, err := term(testContext("req-xread"), map[string]interface{}{
		"command": "cat " + filepath.Join(bob, "SOUL.md"),
	})
	if err != nil || !strings.Contains(res.Content, "bob soul") {
		t.Fatalf("cross-profile read failed: %+v %v", res, err)
	}
	// Writes stay confined with the group message.
	res, err = term(testContext("req-xwrite"), map[string]interface{}{
		"command": "echo x > " + filepath.Join(bob, "notes.md"),
	})
	if err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	if !strings.Contains(res.Content, `"status":"blocked"`) || !strings.Contains(res.Content, "system-managed") {
		t.Fatalf("missing blocked envelope: %q", res.Content)
	}
}

func TestTerminalDeniesCrossProfileStateRead(t *testing.T) {
	_, alice, bob := profileTree(t)
	if err := os.Chmod(alice, 0700); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewWorkspace(alice, "alice")
	term := NewTerminalHandler(ws, process.NewManager(alice))
	res, err := term(testContext("req-xstate"), map[string]interface{}{
		"command": "cd " + bob + " && sqlite3 state.db \"SELECT id FROM sessions;\"",
	})
	if err != nil {
		t.Fatalf("deny returned error: %v", err)
	}
	if !strings.Contains(res.Content, `"status":"blocked"`) || !strings.Contains(res.Content, "session_search") {
		t.Fatalf("missing blocked envelope: %q", res.Content)
	}
}
