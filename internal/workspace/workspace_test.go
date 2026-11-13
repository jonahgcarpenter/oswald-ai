package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func sessionFrom(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "session_id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
		}
	}
	return ""
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
	ctx := requestctx.WithMetadata(testContext("req-cron"), requestctx.Metadata{RequestID: "req-cron", SessionID: "discord:chan1:987654321"})
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
	if stored.Deliver.Platform != "discord" || stored.Deliver.ChatID != "chan1" || stored.Deliver.UserID != "987654321" {
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

func TestCronjobManageZonedTimestamps(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-tz"), requestctx.Metadata{RequestID: "req-cron-tz", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	// Far-future one-shot: asserts the zoned next_run_at rendering (the Nov
	// 2026 wall-clock regression) without depending on the test clock.
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "2027-06-01T00:15:00Z", "prompt": "hi"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	if !strings.Contains(created.Content, "next_run_at: 2027-06-01T00:15:00Z (") || !strings.Contains(created.Content, "UTC") {
		t.Fatalf("create missing zoned next_run_at: %q", created.Content)
	}
	listed, err := manage(ctx, map[string]interface{}{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.Content, "next=2027-06-01T00:15:00Z (") || !strings.Contains(listed.Content, "UTC") {
		t.Fatalf("list missing zoned next: %q", listed.Content)
	}
}

func TestCronjobManageNameAndDeliver(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-name"), requestctx.Metadata{RequestID: "req-cron-name", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	created, err := manage(ctx, map[string]interface{}{"action": "create", "name": "brief", "schedule": "in 1h", "prompt": "hi", "deliver_platform": "discord", "deliver_chat_id": "chan9", "deliver_user_id": "112233445"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "brief" || stored.Deliver.Platform != "discord" || stored.Deliver.ChatID != "chan9" || stored.Deliver.UserID != "112233445" {
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
	ctx := requestctx.WithMetadata(testContext("req-cron-deliver"), requestctx.Metadata{RequestID: "req-cron-deliver", SessionID: "discord:chan1:987654321"})
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
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "deliver_user_id": "alice"}); err == nil {
		t.Fatal("display-name user accepted")
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

func TestCronjobManageMCPServers(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(root, "alice")
	store := cron.NewStore(root)
	ctx := requestctx.WithMetadata(testContext("req-cron-mcp"), requestctx.Metadata{RequestID: "req-cron-mcp", SessionID: "discord:chan1:987654321"})
	manage := NewCronjobManageHandler(ws, store)
	// Omitted exposes all.
	created, err := manage(ctx, map[string]interface{}{"action": "create", "schedule": "in 1h", "prompt": "hi"})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	jobID := strings.TrimSpace(strings.Split(strings.Split(created.Content, "\n")[0], ":")[1])
	stored, err := store.Resolve(jobID)
	if err != nil || stored.MCPServers != nil {
		t.Fatalf("omitted = %+v %v", stored, err)
	}
	// Explicit list scopes.
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "mcp_servers": []interface{}{"homeassistant", "spotify"}}); err != nil {
		t.Fatalf("update = %v", err)
	}
	stored, _ = store.Resolve(jobID)
	if len(stored.MCPServers) != 2 || stored.MCPServers[0] != "homeassistant" {
		t.Fatalf("scoped = %+v", stored.MCPServers)
	}
	// Empty list disables.
	if _, err := manage(ctx, map[string]interface{}{"action": "update", "job_id": jobID, "mcp_servers": []interface{}{}}); err != nil {
		t.Fatalf("clear = %v", err)
	}
	stored, _ = store.Resolve(jobID)
	if stored.MCPServers == nil || len(stored.MCPServers) != 0 {
		t.Fatalf("cleared = %+v, want non-nil empty", stored.MCPServers)
	}
	// List shows the state.
	listed, err := manage(ctx, map[string]interface{}{"action": "list"})
	if err != nil || !strings.Contains(listed.Content, "mcp=none") {
		t.Fatalf("list = %+v %v", listed, err)
	}
	// Bad names and shapes fail closed.
	for _, args := range []map[string]interface{}{
		{"action": "create", "schedule": "in 1h", "prompt": "hi", "mcp_servers": []interface{}{"Spotify"}},
		{"action": "create", "schedule": "in 1h", "prompt": "hi", "mcp_servers": []interface{}{"*"}},
		{"action": "create", "schedule": "in 1h", "prompt": "hi", "mcp_servers": "homeassistant"},
	} {
		if _, err := manage(ctx, args); err == nil {
			t.Fatalf("bad allowlist accepted: %v", args["mcp_servers"])
		}
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
