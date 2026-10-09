package agent

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
)

func TestProcessPersistsGroupExchangeWithoutReasoningOrQuotedReply(t *testing.T) {
	for _, publicText := range []string{"  public question  ", ""} {
		t.Run(publicText, func(t *testing.T) {
			chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "public final answer", Thinking: "private reasoning"}}}}
			a, store := newTestAgent(t, chat, nil, nil)
			defer store.Close()
			meta := requestctx.Metadata{GroupGateway: "discord", GroupChatID: "chat:Exact;ID", PublicUserText: publicText}
			const internalPrompt = "[Replying to a message: \"quoted enrichment\"]\n\ninternal attachment enrichment"
			response, err := a.Process(requestctx.WithMetadata(context.Background(), meta), Request{
				RequestID: "group-request", SessionKey: "group-session", Prompt: internalPrompt,
				Principal: identity.Principal{CanonicalUserID: "user-1", ExternalID: "external-user", Gateway: "discord", Assurance: identity.AssuranceDiscordGateway},
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
			if answer != "public final answer" || !pending || internal == "" || internal == internalPrompt || messagesContain([]llm.ChatMessage{{Content: internal}}, "quoted enrichment") {
				t.Fatalf("incorrect pending exchange: pending=%t", pending)
			}
			if !messagesContain(chat.requests[0].Messages, internalPrompt) {
				t.Fatal("current-turn enrichment was omitted")
			}
		})
	}
}
