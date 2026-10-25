package imessage

import (
	"strings"
	"testing"

	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
)

func TestIMessageClarifyAskSendsThreadedReply(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	defer bb.server.Close()

	r := &runtimeResponder{
		gateway:             g,
		requestID:           "req-clarify",
		chatGUID:            "chat-direct",
		selectedMessageGUID: "msg-9",
		sessionKey:          "imessage:dm:+15551234567",
		senderID:            "+15551234567",
		typingGate:          newTypingGate(),
		typingAllowed:       make(chan struct{}),
	}
	notify := g.clarifyAskNotifier(r, "req-clarify")
	if notify == nil {
		t.Fatal("no clarify asker wired for imessage")
	}
	question := toolclarify.Question{Question: "Q1 - Which color?", Choices: []string{"red", "blue"}}
	if err := notify(toolclarify.Notice{Question: question, MinutesLeft: 5}); err != nil {
		t.Fatalf("clarify ask failed: %v", err)
	}
	if !bb.waitForSentCount(1) {
		t.Fatalf("clarify ask was not sent, got %+v", bb.sentMessages())
	}
	sent := bb.sentMessages()
	if len(sent) != 1 || !strings.Contains(sent[0].Message, "Which color?") || !strings.Contains(sent[0].Message, "A. red (Recommended)") || !strings.Contains(sent[0].Message, "B. blue") || !strings.Contains(sent[0].Message, "C. Other") {
		t.Fatalf("clarify ask malformed: %+v", sent)
	}
	if sent[0].SelectedMessageGUID != "msg-9" {
		t.Fatalf("ask lost reply threading: %+v", sent[0])
	}
}

func TestIMessageClarifyAnswerConsumedPreBroker(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g, b, model := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	toolclarify.Reset()

	chat := messageChat{GUID: "chat-direct", Style: chatStyleDirect}
	sessionKey := g.sessionKey(chat, "+15551234567")
	answer := make(chan toolclarify.Answer, 1)
	if !toolclarify.Request(toolclarify.Pending{Profile: "default", SessionKey: sessionKey, Questions: []toolclarify.Question{{Question: "Which color?", Choices: []string{"red", "blue"}}}, Answer: answer}) {
		t.Fatal("request rejected")
	}
	g.processIncomingMessage(webhookMessage{
		GUID:   "ans-1",
		Text:   "B",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{chat},
	})
	select {
	case ans := <-answer:
		if ans.Text != "B" {
			t.Fatalf("wrong answer delivered: %q", ans.Text)
		}
	default:
		t.Fatal("answer did not reach the parked ask")
	}
	if len(model.primaryRequests()) != 0 {
		t.Fatalf("consumed answer started a turn: %v", model.primaryRequests())
	}
	if n := len(bb.sentMessages()); n != 0 {
		t.Fatalf("consumed answer sent %d messages: %+v", n, bb.sentMessages())
	}
	toolclarify.Reset()
}

func TestIMessageCommandBypassesParkedClarify(t *testing.T) {
	bb := newFakeBlueBubbles(t)
	defer bb.server.Close()
	g, b, _ := newIMessageTestGateway(t, bb.server.URL)
	defer b.Shutdown()
	toolclarify.Reset()

	chat := messageChat{GUID: "chat-direct", Style: chatStyleDirect}
	sessionKey := g.sessionKey(chat, "+15551234567")
	answer := make(chan toolclarify.Answer, 1)
	if !toolclarify.Request(toolclarify.Pending{Profile: "default", SessionKey: sessionKey, Answer: answer}) {
		t.Fatal("request rejected")
	}
	// Unknown commands flow through to the command service instead of
	// fulfilling the ask; the ask stays parked.
	g.processIncomingMessage(webhookMessage{
		GUID:   "cmd-1",
		Text:   "/unknown-command",
		Handle: messageHandle{Address: "+15551234567"},
		Chats:  []messageChat{chat},
	})
	select {
	case <-answer:
		t.Fatal("command text fulfilled the parked ask")
	default:
	}
	if !toolclarify.Has("default", sessionKey) {
		t.Fatal("command consumed the parked ask")
	}
	toolclarify.Reset()
}
