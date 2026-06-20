package runtime

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
)

// Dependencies are the shared services needed to execute a normalized gateway request.
type Dependencies struct {
	Broker   *broker.Broker
	Commands *commands.Router
	Log      *config.Logger
}

// Request is the gateway-neutral representation executed by the shared runtime.
type Request struct {
	RequestID   string
	Gateway     string
	ChatID      string
	SenderID    string
	DisplayName string
	SessionKey  string

	IsDirect     bool
	IsGroup      bool
	IsMention    bool
	IsReplyToBot bool
	IsCommand    bool

	Text        string
	Images      []llm.InputImage
	Unsupported []string
	Reply       *routing.ReplyContext

	StreamFunc func(agent.StreamChunk)
}

// Responder performs gateway-specific delivery and response bookkeeping.
type Responder interface {
	StartProcessing() (func(), error)
	SendFallback(text string) error
	SendCommandResponse(text string) error
	SendAgentResponse(response *agent.AgentResponse) error
	SendAgentError(text string) error
}

// Outcome describes how a normalized request was handled.
type Outcome struct {
	Action routing.Action
	Reason string
	Err    error
}
