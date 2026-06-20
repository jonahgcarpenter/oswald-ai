package imessage

import (
	gatewayruntime "github.com/jonahgcarpenter/oswald-ai/internal/gateway/runtime"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
)

type decisionHooks struct {
	gateway            *Gateway
	message            webhookMessage
	sessionKey         string
	normalizedSenderID string
	displayName        string
}

func (h *decisionHooks) OnDecision(_ gatewayruntime.Request, _ routing.Decision) {
	h.gateway.rememberInboundMessage(h.message, h.sessionKey, h.normalizedSenderID, h.displayName)
}
