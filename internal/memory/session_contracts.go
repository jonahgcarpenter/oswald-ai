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
	// Images contains at most four normalized generated outputs, oldest first.
	Images []requestctx.InputImage
}
