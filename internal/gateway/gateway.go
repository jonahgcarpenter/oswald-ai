package gateway

import "github.com/jonahgcarpenter/oswald-ai/internal/agent"

type Service interface {
	// Start should block and run the gateway.
	// Returning an error means it crashed or failed to start.
	Start(aiAgent *agent.Agent) error
	Name() string
}

// AgentRequest is reserved for future request routing across multiple gateways.
// TODO: Implement a message broker/multiplexer to route responses back to the correct gateway.
// Currently, messages are not routed by gateway (responses are broadcast or directed by context).
// This structure prepares for multi-gateway request attribution.
type AgentRequest struct {
	Channel    string // Gateway name (e.g., "discord", "websocket")
	ChatID     string // Conversation/room identifier
	SenderID   string // User identifier
	SessionKey string // Unique conversation context identifier
}
