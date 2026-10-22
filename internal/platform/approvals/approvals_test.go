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

func TestNotifySignalsWaiter(t *testing.T) {
	Reset()
	defer Reset()
	decide := make(chan Decision, 1)
	if !Request(Pending{Profile: "alice", SessionKey: "s4", Command: "echo x", Decide: decide}) {
		t.Fatal("request refused")
	}
	got, ok, expired := Take("alice", "s4")
	if !ok || expired {
		t.Fatalf("take = ok=%t expired=%t", ok, expired)
	}
	if !Notify(got, DecisionApproved) {
		t.Fatal("notify refused")
	}
	select {
	case d := <-decide:
		if d != DecisionApproved {
			t.Fatalf("decision = %q", d)
		}
	default:
		t.Fatal("waiter channel empty")
	}
	if Notify(Pending{}, DecisionDenied) {
		t.Fatal("notify without channel succeeded")
	}
}

func TestDenySessionUnblocksWaiter(t *testing.T) {
	Reset()
	defer Reset()
	decide := make(chan Decision, 1)
	if !Request(Pending{Profile: "alice", SessionKey: "s5", Command: "echo x", Decide: decide}) {
		t.Fatal("request refused")
	}
	if !DenySession("alice", "s5") {
		t.Fatal("deny missed ask")
	}
	select {
	case d := <-decide:
		if d != DecisionDenied {
			t.Fatalf("decision = %q", d)
		}
	default:
		t.Fatal("waiter was not unblocked")
	}
}

func TestExpiredTakeUnblocksWaiter(t *testing.T) {
	Reset()
	defer Reset()
	old := ApprovalWindow
	ApprovalWindow = 30 * time.Millisecond
	defer func() { ApprovalWindow = old }()
	decide := make(chan Decision, 1)
	if !Request(Pending{Profile: "alice", SessionKey: "s6", Command: "echo x", Decide: decide}) {
		t.Fatal("request refused")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok, expired := Take("alice", "s6"); ok || !expired {
		t.Fatalf("expired take = ok=%t expired=%t", ok, expired)
	}
	select {
	case d := <-decide:
		if d != DecisionDenied {
			t.Fatalf("decision = %q", d)
		}
	default:
		t.Fatal("expired waiter was not unblocked")
	}
}
