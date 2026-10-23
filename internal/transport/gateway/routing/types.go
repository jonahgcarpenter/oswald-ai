package routing

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
)

// Action describes how a gateway should handle a normalized inbound message.
type Action string

const (
	// ActionIgnore means the gateway should drop the message silently.
	ActionIgnore Action = "ignore"
	// ActionLLM means the gateway should submit the request to the broker.
	ActionLLM Action = "llm"
	// ActionCommand means the gateway should handle a command.
	ActionCommand Action = "command"
	// ActionGatewayFallback means the gateway should send ResponseText directly.
	ActionGatewayFallback Action = "gateway_fallback"
)

// Input is the gateway-neutral representation of an inbound user message.
type Input struct {
	IsGroup            bool
	IsMention          bool
	IsReplyToBot       bool
	IsCommandAttempt   bool
	Text               string
	CurrentImages      []llm.InputImage
	CurrentUnsupported []string
	CurrentDocuments   []media.DocumentRef
	Reply              *ReplyContext
}

// PreflightInput is the cheap gateway-neutral subset needed for early ignores.
type PreflightInput struct {
	IsGroup      bool
	IsMention    bool
	IsReplyToBot bool
	Text         string
}

// ReplyContext is the gateway-neutral representation of a replied-to message.
type ReplyContext struct {
	SenderName            string
	Text                  string
	IsFromBot             bool
	Images                []llm.InputImage
	Unsupported           []string
	Documents             []media.DocumentRef
	IsUnavailable         bool
	AttachmentUnavailable bool
}

// Decision is the canonical routing result gateways execute.
type Decision struct {
	Action       Action
	Prompt       string
	Images       []llm.InputImage
	Documents    []media.DocumentRef
	ResponseText string
	Reason       string
}
