package memory

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"time"
)

// SessionContext identifies an active profile conversation generation.
type SessionContext struct {
	Generation   int
	SpeakerIntro string
	StartedAt    time.Time
	IsNewSession bool
}

// SessionTurnWrite contains one pending exchange and its immutable artifacts.
type SessionTurnWrite struct {
	SessionID, UserID                         string
	Generation                                int
	UserText, AssistantText                   string
	GroupGateway, GroupChatID, PublicUserText string
	ToolNames                                 []string
	History                                   ToolHistory
	TTL                                       time.Duration
	Pressure                                  SessionPromptPressure
	// AssistantFinishReason is the provider stop reason for the final
	// assistant message (for example "stop"); empty defaults to "stop".
	AssistantFinishReason string
	// AssistantReasoning and AssistantReasoningContent carry the final
	// model thinking traces persisted on the assistant message row.
	AssistantReasoning, AssistantReasoningContent string
	// AssistantTokenCount is the final completion token count; zero omits it.
	AssistantTokenCount int
	// UserPlatformMessageID is the untrusted inbound transport message
	// identifier persisted on the user message row; empty omits it.
	UserPlatformMessageID string
	// Model, BillingProvider, BillingBaseURL describe the chat model and
	// endpoint attributed on the session row.
	Model, BillingProvider, BillingBaseURL string
	// ModelConfig is provider config JSON stored on the session row.
	ModelConfig string
	// ChatID, ChatType carry gateway conversation identity for the session
	// row; empty values omit them.
	ChatID, ChatType string
	// ChatDisplayName is the untrusted plain conversation name recorded in
	// the session origin record; empty omits it.
	ChatDisplayName string
	// Platform is the untrusted transport name (for example discord) recorded
	// in the session origin record; empty omits it.
	Platform string
	// TransportProfile names the profile owning the receiving adapter,
	// credential, and allowlist for the session row; empty omits it. It never
	// affects session ownership.
	TransportProfile string
	// PlatformUserID is the transport user identifier (Discord author id,
	// iMessage sender) stored on the session row; empty omits it. It never
	// affects ownership, which stays with UserID.
	PlatformUserID string
	// PlatformDisplayName is the transport sender display name stored on the
	// session row; empty omits it.
	PlatformDisplayName string
	// SystemPromptHash is the hex SHA-256 of SystemPromptText, persisted with
	// the raw text into the system prompt ledger.
	SystemPromptHash, SystemPromptText string
	// ActivityDescription notes the turn outcome on the session row.
	ActivityDescription string
	// Images contains at most four normalized generated outputs, oldest first.
	Images []requestctx.InputImage
}
