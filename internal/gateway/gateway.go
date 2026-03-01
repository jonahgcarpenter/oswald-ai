package gateway

import "github.com/jonahgcarpenter/oswald-ai/internal/agent"

type Service interface {
	// Start should block and run the gateway.
	// Returning an error means it crashed or failed to start.
	Start(aiAgent *agent.Agent) error
	Name() string
}

// TODO: I need to create an middleman to aviod cross gateway contamination
// This way every message is directed to where it came from
type AgentRequest struct {
	Channel    string // Name of gateway
	ChatID     string // The specific  user ID.
	SenderID   string // The specific room.
	SessionKey string // A unique identifier for the conversation context.
}
