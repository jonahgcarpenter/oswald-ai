package runtime

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// Dependencies are the shared services needed to execute a normalized gateway request.
type Dependencies struct {
	Broker    *broker.Broker
	Commands  *commands.Service
	Access    AccessChecker
	Log       *config.Logger
	Formation FormationEnqueuer
}

// FormationEnqueuer durably queues optional work after response delivery.
type FormationEnqueuer interface {
	Enqueue(context.Context, string, usermemory.FormationSource) error
	MarkConfirmationPresented(context.Context, string, string, int, string, int64) error
}

// AccessChecker exposes gateway-neutral user moderation checks.
type AccessChecker interface {
	BanStatus(canonicalUserID string) (bool, string, error)
}

// Request is the gateway-neutral representation executed by the shared runtime.
type Request struct {
	RequestID   string
	ChatID      string
	Principal   identity.Principal
	DisplayName string
	SessionKey  string

	IsDirect     bool
	IsGroup      bool
	IsMention    bool
	IsReplyToBot bool

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
