package discord

import (
	"time"

	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
)

type decisionHooks struct {
	gateway     *Gateway
	messageID   string
	sessionKey  string
	channelID   string
	senderID    string
	displayName string
	text        string
	attachments []Attachment
	embeds      []Embed
}

func (h *decisionHooks) OnDecision(_ gatewayruntime.Request, _ routing.Decision) {
	h.gateway.rememberReply(h.messageID, replyContext{
		SessionKey:  h.sessionKey,
		ChannelID:   h.channelID,
		SenderID:    h.senderID,
		DisplayName: h.displayName,
		Text:        h.text,
		Attachments: h.attachments,
		Embeds:      h.embeds,
		IsFromBot:   false,
		CreatedAt:   time.Now(),
	})
}
