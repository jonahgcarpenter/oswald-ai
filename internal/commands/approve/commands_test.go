package approve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/approvals"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

func testRequest(root, session, raw string) commands.Request {
	return commands.Request{
		Principal:  identity.Principal{CanonicalUserID: "alice", Gateway: "discord", ExternalID: "123", Assurance: identity.AssuranceDiscordGateway},
		SessionKey: session,
		Raw:        raw,
	}
}

func TestApproveExecutesPending(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if !approvals.Request(approvals.Pending{Profile: "alice", SessionKey: "s1", Command: "echo approved-hi", Workdir: root, Scratch: filepath.Join(root, "cache", "scratch"), Timeout: 30}) {
		t.Fatal("request refused")
	}
	result, err := NewApprove(root).Execute(context.Background(), testRequest(root, "s1", "/approve"))
	if err != nil || !strings.Contains(result.Text, "approved-hi") {
		t.Fatalf("approve = %+v %v", result, err)
	}
}

func TestDenyConsumesPending(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	approvals.Request(approvals.Pending{Profile: "alice", SessionKey: "s2", Command: "rm -rf /", Workdir: root, Timeout: 30})
	result, err := NewDeny().Execute(context.Background(), testRequest(root, "s2", "/deny"))
	if err != nil || !strings.Contains(result.Text, "Denied") {
		t.Fatalf("deny = %+v %v", result, err)
	}
	again, err := NewDeny().Execute(context.Background(), testRequest(root, "s2", "/deny"))
	if err != nil || !strings.Contains(again.Text, "No command is waiting") {
		t.Fatalf("second deny = %+v %v", again, err)
	}
}

func TestApproveReportsExpiryAndAbsence(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	result, err := NewApprove(root).Execute(context.Background(), testRequest(root, "s3", "/approve"))
	if err != nil || !strings.Contains(result.Text, "No command is waiting") {
		t.Fatalf("absent approve = %+v %v", result, err)
	}
	old := approvals.ApprovalWindow
	approvals.ApprovalWindow = 0
	approvals.Request(approvals.Pending{Profile: "alice", SessionKey: "s4", Command: "echo x", Workdir: root, Timeout: 30})
	approvals.ApprovalWindow = old
	expired, err := NewApprove(root).Execute(context.Background(), testRequest(root, "s4", "/approve"))
	if err != nil || !strings.Contains(expired.Text, "expired") {
		t.Fatalf("expired approve = %+v %v", expired, err)
	}
}

func TestApproveRejectsArgsAndStrangers(t *testing.T) {
	approvals.Reset()
	defer approvals.Reset()
	root := t.TempDir()
	req := testRequest(root, "s5", "/approve extra")
	req.Args = []string{"extra"}
	result, err := NewApprove(root).Execute(context.Background(), req)
	if err != nil || !strings.Contains(result.Text, "Use:") {
		t.Fatalf("args approve = %+v %v", result, err)
	}
	anon := testRequest(root, "s5", "/deny")
	anon.Principal = identity.Principal{}
	denied, err := NewDeny().Execute(context.Background(), anon)
	if err != nil || !strings.Contains(denied.Text, "authenticated") {
		t.Fatalf("anon deny = %+v %v", denied, err)
	}
	// Other conversations cannot consume this session's ask.
	approvals.Request(approvals.Pending{Profile: "alice", SessionKey: "s6", Command: "echo x", Workdir: root, Timeout: 30})
	other, err := NewDeny().Execute(context.Background(), testRequest(root, "other", "/deny"))
	if err != nil || !strings.Contains(other.Text, "No command is waiting") {
		t.Fatalf("cross-session deny = %+v %v", other, err)
	}
}
