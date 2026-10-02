package memory

import (
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"time"
)

// SessionContext identifies an active profile conversation generation.
type SessionContext struct {
	Generation   int
	SpeakerIntro string
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
	// Images contains at most four normalized generated outputs, oldest first.
	Images []requestctx.InputImage
}
