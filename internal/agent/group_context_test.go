package agent

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestProcessRetainsReplyContextAcrossGatewayTurns(t *testing.T) {
	for _, gateway := range []string{"discord", "imessage"} {
		for _, group := range []bool{false, true} {
			t.Run(gateway+map[bool]string{false: "/direct", true: "/group"}[group], func(t *testing.T) {
				chat := &fakeChatter{responses: []*llm.ChatResponse{
					{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "public final answer", Thinking: "private reasoning"}},
					{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "follow-up answer"}},
				}}
				a, store := newTestAgent(t, chat, nil, nil)
				defer store.Close()
				var meta requestctx.Metadata
				if group {
					meta = requestctx.Metadata{GroupGateway: gateway, GroupChatID: "chat:Exact;ID", PublicUserText: "new question"}
				}
				assurance := identity.AssuranceDiscordGateway
				if gateway == "imessage" {
					assurance = identity.AssuranceBlueBubblesWebhook
				}
				principal := identity.Principal{CanonicalUserID: "user-1", ExternalID: "external-user", Gateway: gateway, Assurance: assurance}
				const internalPrompt = "[Replying to Oswald: \"Complete quoted answer.\\n\\nSecond paragraph.\"]\n\nnew question"
				response, err := a.Process(requestctx.WithMetadata(context.Background(), meta), Request{
					RequestID: "group-request", SessionKey: "group-session", Prompt: internalPrompt,
					Principal: principal,
				})
				if err != nil {
					t.Fatal(err)
				}
				if response.SourceTurnID <= 0 {
					t.Fatal("missing persisted turn")
				}
				var internal, answer string
				var pending bool
				err = store.sql.QueryRow(`SELECT u.content,a.content,a.active=0 FROM messages a JOIN state_meta e ON e.key='oswald:v1:turn:'||a.session_id||':'||a.id JOIN messages u ON u.id=json_extract(e.value,'$.user_message_id') WHERE a.id=?`, response.SourceTurnID).Scan(&internal, &answer, &pending)
				if err != nil {
					t.Fatal(err)
				}
				if answer != "public final answer" || !pending || internal != internalPrompt {
					t.Fatalf("incorrect pending exchange: pending=%t", pending)
				}
				if !messagesContain(chat.requests[0].Messages, internalPrompt) {
					t.Fatal("current-turn enrichment was omitted")
				}
				if err := store.MarkSessionTurnDelivered(context.Background(), "user-1", response.SourceTurnID); err != nil {
					t.Fatal(err)
				}
				if _, err := a.Process(context.Background(), Request{
					RequestID: "follow-up", SessionKey: "group-session", Prompt: "What was I replying to?", Principal: principal,
				}); err != nil {
					t.Fatal(err)
				}
				if len(chat.requests) != 2 {
					t.Fatalf("model requests=%d", len(chat.requests))
				}
				if !messagesContain(chat.requests[1].Messages, internalPrompt) {
					t.Fatal("delivered reply context missing from the next turn")
				}
				if messagesContain(chat.requests[1].Messages, "private reasoning") {
					t.Fatal("reasoning replayed in history")
				}
			})
		}
	}
}
