package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
)

// TestInflightTracksAdmittedTurns covers the issue-258 contract: the broker
// holds the platform message ID of each admitted turn from enqueue until the
// turn settles, so gateways can key reply-to-bot admission to invocation.
func TestInflightTracksAdmittedTurns(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 1)}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()

	principal := identity.Principal{CanonicalUserID: "user", Gateway: "imessage", ExternalID: "user", Assurance: identity.AssuranceBlueBubblesWebhook}
	req := &Request{
		Principal:         principal,
		SessionKey:        "session",
		ChatID:            "chat;+;group",
		PlatformMessageID: "invoke-1",
		Prompt:            "hello",
		RequestID:         "req-1",
		ResponseChan:      make(chan Result, 1),
	}
	if err := b.Submit(req); err != nil {
		t.Fatal(err)
	}
	// Enqueue registers before dispatch; the ID must be visible while the
	// turn is queued or running.
	deadline := time.Now().Add(2 * time.Second)
	for !b.IsInflight("chat;+;group", "invoke-1") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !b.IsInflight("chat;+;group", "invoke-1") {
		t.Fatal("admitted turn was not tracked as in-flight")
	}
	select {
	case result := <-req.ResponseChan:
		if result.Err != nil {
			t.Fatalf("request failed: %v", result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admitted turn never completed")
	}
	// Release happens when the turn completes.
	deadline = time.Now().Add(2 * time.Second)
	for b.IsInflight("chat;+;group", "invoke-1") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.IsInflight("chat;+;group", "invoke-1") {
		t.Fatal("completed turn was not released from the in-flight set")
	}
}

func TestInflightEmptyIDsNeverMatch(t *testing.T) {
	b := NewBroker(nil, 1, config.NewLogger(config.LevelError))
	defer b.Shutdown()
	if b.IsInflight("", "invoke-1") || b.IsInflight("chat", "") || b.IsInflight(" ", " ") {
		t.Fatal("empty chat or message IDs must never match")
	}
}

func TestInflightReleasedOnCancel(t *testing.T) {
	processor := &cancelProcessor{started: make(chan struct{})}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	b.Start()
	defer b.Shutdown()

	principal := identity.Principal{CanonicalUserID: "user", Gateway: "imessage", ExternalID: "user", Assurance: identity.AssuranceBlueBubblesWebhook}
	req := &Request{
		Principal:         principal,
		SessionKey:        "session",
		ChatID:            "chat;+;group",
		PlatformMessageID: "invoke-cancel",
		Prompt:            "hello",
		RequestID:         "req-cancel",
		Ctx:               context.Background(),
		ResponseChan:      make(chan Result, 1),
	}
	done := make(chan error, 1)
	go func() { done <- b.Submit(req) }()
	select {
	case <-processor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking turn never started")
	}
	if !b.IsInflight("chat;+;group", "invoke-cancel") {
		t.Fatal("running turn was not tracked as in-flight")
	}
	b.CancelAllAgentWork()
	select {
	case result := <-req.ResponseChan:
		// Immediate cancellation of still-running work reports
		// ExecutionComplete=false by contract; the release of the
		// in-flight registration is what matters here.
		if !errors.Is(result.Err, ErrAgentWorkCanceled) {
			t.Fatalf("canceled result = %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled turn never settled")
	}
	if b.IsInflight("chat;+;group", "invoke-cancel") {
		t.Fatal("canceled turn was not released from the in-flight set")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("submit returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submit never returned")
	}
}

func TestInflightIsolatedByChat(t *testing.T) {
	processor := &captureProcessor{requests: make(chan agent.Request, 2)}
	b := NewBroker(processor, 1, config.NewLogger(config.LevelError))
	// Do not start: works stay queued so both IDs remain tracked for the
	// isolation check without racing completion.
	defer b.Shutdown()

	principal := identity.Principal{CanonicalUserID: "user", Gateway: "imessage", ExternalID: "user", Assurance: identity.AssuranceBlueBubblesWebhook}
	mkReq := func(chat, platform, requestID string) *Request {
		return &Request{
			Principal:         principal,
			SessionKey:        "session-" + chat,
			ChatID:            chat,
			PlatformMessageID: platform,
			Prompt:            "hello",
			RequestID:         requestID,
			ResponseChan:      make(chan Result, 1),
		}
	}
	if err := b.Submit(mkReq("chat-a", "shared-guid", "req-a")); err != nil {
		t.Fatal(err)
	}
	if !b.IsInflight("chat-a", "shared-guid") {
		t.Fatal("first turn was not tracked")
	}
	// Same platform GUID in another chat must not match.
	if b.IsInflight("chat-b", "shared-guid") {
		t.Fatal("in-flight lookup crossed chats")
	}
	if b.IsInflight("chat-a", "other-guid") {
		t.Fatal("in-flight lookup matched an unknown message")
	}
}
