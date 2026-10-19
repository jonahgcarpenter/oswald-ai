package memory

import (
	"time"
)

// DeliverySource carries delivered exchange correlation into delivery marking.
type DeliverySource struct {
	RequestID, SessionID string
	SessionGeneration    int
	TurnID               int64
	Model                string
}

// StoredSessionTurn identifies an exchange actually persisted, not delivered.
type StoredSessionTurn struct {
	ID                int64
	UserID, SessionID string
	Generation        int
	UserText          string
	CreatedAt         time.Time
	AssistantResponse string
}

// SessionPromptPressure becomes planner-visible only after successful delivery.
type SessionPromptPressure struct {
	Tokens, Limit int
	Version       string
}

// SessionTurn is a complete exchange stored for conversation continuity.
type SessionTurn struct {
	ID                      int64
	SessionID, UserID       string
	Generation              int
	UserText, AssistantText string
	ToolNames               []string
	ToolHistory             ToolHistory
	CreatedAt, ExpiresAt    time.Time
}
