package runtime

import (
	"context"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/broker"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/gateway/routing"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

// Dependencies are the shared services needed to execute a normalized gateway request.
type Dependencies struct {
	// ForProfile selects immutable profile services before any runtime work.
	ForProfile func(string) (Dependencies, bool)
	Broker     *broker.Broker
	Commands   *commands.Service
	Log        *config.Logger
	Compaction CompactionEnqueuer
}

// CompactionEnqueuer durably plans optional session compaction after delivery.
type CompactionEnqueuer interface {
	Enqueue(context.Context, string, memory.DeliverySource) error
	MarkDeliveryFailed(context.Context, string, int64) error
}

// Request is the gateway-neutral representation executed by the shared runtime.
type Request struct {
	// ReceivedAt is transport receipt time; zero falls back to runtime entry time.
	ReceivedAt  time.Time
	RequestID   string
	ChatID      string
	Principal   identity.Principal
	DisplayName string
	SessionKey  string
	ClientID    string
	// ChatLabel names the transport conversation for the session-context block.
	// It is untrusted metadata, built by each gateway.
	ChatLabel string

	IsDirect     bool
	IsGroup      bool
	IsMention    bool
	IsReplyToBot bool

	// PublicUserText is the exact inbound transport text, captured before any
	// mention, emoji, URL, reply, or attachment transformations. Empty stays empty.
	PublicUserText string
	Text           string
	// PlatformMessageID is the untrusted inbound transport message identifier
	// (Discord message id, BlueBubbles message GUID). Empty when unavailable.
	PlatformMessageID string
	// ChatDisplayName is the untrusted plain conversation name recorded in the
	// session origin record. Empty omits it.
	ChatDisplayName string
	// TransportProfile names the profile owning the receiving adapter,
	// credential, and allowlist. Adapters are process-global today, so inbound
	// gateway traffic always arrives as the reserved default profile.
	TransportProfile string
	// Stateless uses only ClientHistory for prior conversation context and does not persist this turn.
	Stateless     bool
	ClientHistory []llm.ChatMessage
	// Context carries transport cancellation (for example an HTTP client
	// disconnect). Nil means no transport deadline; commands and gateways
	// without a transport context leave it nil so that work outlives the
	// transport as before.
	Context context.Context
	// ClientTools are caller-supplied function definitions combined with Oswald
	// tools for this request only. They are never executed server-side.
	ClientTools []llm.Tool
	// ClientToolChoice is the normalized caller preference. See
	// agent.ClientToolChoice for the "none", "required", named, and
	// "allowed_tools" modes applied to the merged catalog.
	ClientToolChoice agent.ClientToolChoice
	// ClientParallelToolCalls carries the caller's explicit
	// parallel_tool_calls preference through to the agent.
	ClientParallelToolCalls *bool
	// ClientGeneration carries the caller's sampling and stop parameters
	// through to the agent for stateless requests.
	ClientGeneration *agent.ClientGeneration
	// ServiceTier carries the caller's validated service_tier value through
	// to the agent for stateless requests.
	ServiceTier string
	Images      []llm.InputImage
	Documents   []media.DocumentRef
	Unsupported []string
	Reply       *routing.ReplyContext

	StreamFunc func(agent.StreamChunk)
	// EscalationNotify reports a parked approval ask while the tool call
	// blocks. Streaming transports leave this unset and render the agent's
	// status chunk instead; transports without live progress send a real
	// message through this hook.
	EscalationNotify workspace.EscalationNotifier
	// OnAllowed runs after profile authentication, before fallback, command, or model work.
	OnAllowed func()
}

// Responder performs gateway-specific delivery and response bookkeeping.
type Responder interface {
	StartProcessing() (func(), error)
	SendFallback(text string) error
	SendCommandResponse(result commands.Result) error
	SendAgentResponse(response *agent.Response) error
	SendAgentError(text string) error
}

// CancellationResponder performs optional transport-specific cleanup for a stopped request.
type CancellationResponder interface {
	CancelAgentResponse() error
}

// CommandStatusStreamer is an optional responder capability for non-terminal
// command progress. Unlike SendCommandResponse, Stream does not complete the
// lifecycle message, so a later SendCommandResponse still delivers.
type CommandStatusStreamer interface {
	Stream(agent.StreamChunk)
}

// Outcome describes how a normalized request was handled.
type Outcome struct {
	Action routing.Action
	Reason string
	Err    error
}
