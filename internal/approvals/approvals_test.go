package approvals

import (
	"testing"
	"time"
)

func TestRequestSingleFlightAndExpiry(t *testing.T) {
	Reset()
	defer Reset()
	old := ApprovalWindow
	ApprovalWindow = 50 * time.Millisecond
	defer func() { ApprovalWindow = old }()

	first := Pending{Profile: "alice", SessionKey: "s1", Command: "echo one"}
	if !Request(first) {
		t.Fatal("first request refused")
	}
	if Request(Pending{Profile: "alice", SessionKey: "s1", Command: "echo two"}) {
		t.Fatal("second request replaced pending")
	}
	if !Has("alice", "s1") {
		t.Fatal("pending not visible")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok, expired := Take("alice", "s1"); ok || !expired {
		t.Fatalf("expired take = ok=%t expired=%t", ok, expired)
	}
}

func TestTakeConsumeOnce(t *testing.T) {
	Reset()
	defer Reset()
	if !Request(Pending{Profile: "alice", SessionKey: "s2", Command: "echo hi"}) {
		t.Fatal("request refused")
	}
	got, ok, expired := Take("alice", "s2")
	if !ok || expired || got.Command != "echo hi" {
		t.Fatalf("take = %+v ok=%t expired=%t", got, ok, expired)
	}
	if _, ok, _ := Take("alice", "s2"); ok {
		t.Fatal("double take succeeded")
	}
}

func TestDenySession(t *testing.T) {
	Reset()
	defer Reset()
	if DenySession("alice", "s3") {
		t.Fatal("deny reported missing ask")
	}
	Request(Pending{Profile: "alice", SessionKey: "s3", Command: "echo x"})
	if !DenySession("alice", "s3") {
		t.Fatal("deny missed ask")
	}
	if _, ok, _ := Take("alice", "s3"); ok {
		t.Fatal("denied ask still consumable")
	}
}
