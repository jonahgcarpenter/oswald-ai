package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/approvals"
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
