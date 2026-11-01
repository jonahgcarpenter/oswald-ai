package imessage

import (
	"net/http"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

func countTypingDeletes(bb *fakeBlueBubbles, path string) int {
	count := 0
	for _, req := range bb.orderedMethodPaths() {
		if req.method == http.MethodDelete && req.path == path {
			count++
		}
	}
	return count
}

func countTypingPosts(bb *fakeBlueBubbles, path string) int {
	count := 0
	for _, req := range bb.orderedMethodPaths() {
		if req.method == http.MethodPost && req.path == path {
			count++
		}
	}
	return count
}

func closedAllowed() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func finishedGate() *typingGate {
	gate := newTypingGate()
	gate.finish()
	return gate
}

// Sends assume the server auto-clears: no DELETE fires at send time. The
// single backup DELETE arrives after the delay, and shutdown flushes a
// pending backup immediately.
func TestIMessageBackupStopOnlyAfterDelay(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g := &Gateway{BlueBubblesURL: bb.server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext), typingBackupAfter: 30 * time.Second}
	r := &runtimeResponder{gateway: g, requestID: "backup-test", chatGUID: "chat-direct", sessionKey: "session", senderID: "sender", typingGate: finishedGate(), typingAllowed: closedAllowed()}

	const typingPath = "/api/v1/chat/chat-direct/typing"
	if err := r.SendAgentResponse(&agent.Response{Response: "hello"}); err != nil {
		t.Fatal(err)
	}
	if !bb.waitForSentCount(1) {
		t.Fatal("expected agent response to be sent")
	}
	time.Sleep(300 * time.Millisecond)
	if got := countTypingDeletes(bb, typingPath); got != 0 {
		t.Fatalf("expected no immediate typing DELETE after send, got %d in %+v", got, bb.orderedMethodPaths())
	}
	g.StopOutbound()
	if !bb.waitForMethodPath(http.MethodDelete, typingPath) {
		t.Fatalf("expected shutdown to flush the pending backup stop, got %+v", bb.orderedMethodPaths())
	}
}

// A new turn's POST supersedes the previous turn's pending backup: only one
// DELETE fires across both turns.
func TestIMessageNewTurnCancelsPendingBackup(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g := &Gateway{BlueBubblesURL: bb.server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext), typingBackupAfter: 150 * time.Millisecond}

	const typingPath = "/api/v1/chat/chat-direct/typing"
	r1 := &runtimeResponder{gateway: g, requestID: "req-1", chatGUID: "chat-direct", sessionKey: "session", senderID: "sender", typingGate: finishedGate(), typingAllowed: closedAllowed()}
	if err := r1.SendAgentResponse(&agent.Response{Response: "first"}); err != nil {
		t.Fatal(err)
	}
	// Wait for the first turn's backup to be armed before the next turn starts.
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.typingMu.Lock()
		armed := len(g.typingTimers) == 1
		g.typingMu.Unlock()
		if armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first turn never armed its backup stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
	postsBefore := countTypingPosts(bb, typingPath)
	g.startProcessingIndicators("chat-direct", "req-2", newTypingGate())
	deadline = time.Now().Add(2 * time.Second)
	for countTypingPosts(bb, typingPath) <= postsBefore {
		if time.Now().After(deadline) {
			t.Fatal("second turn never posted typing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r2 := &runtimeResponder{gateway: g, requestID: "req-2", chatGUID: "chat-direct", sessionKey: "session", senderID: "sender", typingGate: finishedGate(), typingAllowed: closedAllowed()}
	if err := r2.SendAgentResponse(&agent.Response{Response: "second"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for countTypingDeletes(bb, typingPath) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("expected the surviving backup stop, got %+v", bb.orderedMethodPaths())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	if got := countTypingDeletes(bb, typingPath); got != 1 {
		t.Fatalf("expected exactly 1 backup DELETE across both turns, got %d in %+v", got, bb.orderedMethodPaths())
	}
}

// Shutdown deliberately clears chats with a live indicator even though no
// terminal send happened yet.
func TestIMessageShutdownStopsInflightTyping(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g := &Gateway{BlueBubblesURL: bb.server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext)}

	const typingPath = "/api/v1/chat/chat-direct/typing"
	g.startProcessingIndicators("chat-direct", "req-inflight", newTypingGate())
	if !bb.waitForMethodPath(http.MethodPost, typingPath) {
		t.Fatalf("expected typing start, got %+v", bb.orderedMethodPaths())
	}
	g.StopOutbound()
	if !bb.waitForMethodPath(http.MethodDelete, typingPath) {
		t.Fatalf("expected shutdown to delete the in-flight indicator, got %+v", bb.orderedMethodPaths())
	}
}

// A cancel sends its DELETE deliberately without waiting for the backup delay.
func TestIMessageCancelSendsDeliberateStop(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g := &Gateway{BlueBubblesURL: bb.server.URL, BlueBubblesPassword: "pw", Log: config.NewLogger(config.LevelError), messageIndex: make(map[string]messageContext), typingBackupAfter: 30 * time.Second}
	r := &runtimeResponder{gateway: g, requestID: "cancel-test", chatGUID: "chat-direct", sessionKey: "session", senderID: "sender", typingGate: finishedGate(), typingAllowed: closedAllowed()}

	const typingPath = "/api/v1/chat/chat-direct/typing"
	if err := r.CancelAgentResponse(); err != nil {
		t.Fatal(err)
	}
	if !bb.waitForMethodPath(http.MethodDelete, typingPath) {
		t.Fatalf("expected deliberate typing DELETE on cancel, got %+v", bb.orderedMethodPaths())
	}
	g.StopOutbound()
}
