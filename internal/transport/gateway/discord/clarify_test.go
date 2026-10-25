package discord

import (
	"testing"
	"time"

	toolclarify "github.com/jonahgcarpenter/oswald-ai/internal/tools/clarify"
)

func TestDiscordClarifyAnswerConsumedPreBroker(t *testing.T) {
	dg, b, chat := newDiscordTestGateway(t, "")
	defer b.Shutdown()
	toolclarify.Reset()

	answer := make(chan toolclarify.Answer, 1)
	if !toolclarify.Request(toolclarify.Pending{Profile: "default", SessionKey: "discord:dm:123", Questions: []toolclarify.Question{{Question: "Which color?"}}, Answer: answer}) {
		t.Fatal("request rejected")
	}
	dg.handleReceivedMessage(discordMessage("ans-1", "channel-1", "", "123", "frag", "1: blue"), "req-clarify", time.Now())
	select {
	case ans := <-answer:
		if ans.Text != "1: blue" {
			t.Fatalf("wrong answer delivered: %q", ans.Text)
		}
	default:
		t.Fatal("answer did not reach the parked ask")
	}
	if len(primaryDiscordRequests(chat.requests)) != 0 {
		t.Fatalf("consumed answer started a turn: %v", chat.requests)
	}
	toolclarify.Reset()
}

func TestDiscordCommandBypassesParkedClarify(t *testing.T) {
	dg, b, _ := newDiscordTestGateway(t, "")
	defer b.Shutdown()
	toolclarify.Reset()

	answer := make(chan toolclarify.Answer, 1)
	if !toolclarify.Request(toolclarify.Pending{Profile: "default", SessionKey: "discord:dm:123", Answer: answer}) {
		t.Fatal("request rejected")
	}
	// Commands flow through to the command service instead of fulfilling
	// the ask; the ask stays parked.
	dg.handleReceivedMessage(discordMessage("cmd-1", "channel-1", "", "123", "frag", "/stop"), "req-command", time.Now())
	select {
	case <-answer:
		t.Fatal("command text fulfilled the parked ask")
	default:
	}
	if !toolclarify.Has("default", "discord:dm:123") {
		t.Fatal("command consumed the parked ask")
	}
	toolclarify.Reset()
}
