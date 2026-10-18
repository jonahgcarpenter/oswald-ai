package memory

import (
	"errors"
	"time"
)

// ErrModelSubmissionBudgetExhausted identifies exhausted durable provider credits.
var ErrModelSubmissionBudgetExhausted = errors.New("durable model submission budget exhausted")

// ErrStaleSessionCompactionJobLease identifies a lost exact-token compression lease.
var ErrStaleSessionCompactionJobLease = errors.New("stale session compaction job lease")

// DeliverySource carries delivered exchange correlation into compression.
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

const (
	SessionCompactionModelSubmissionLimit    = 4
	SessionCompactionInvalidOutputRetryLimit = 3
	maxSummaryNarrativeRunes                 = 8000
	maxSummaryArrayItems                     = 50
	maxSummaryItemRunes                      = 1000
	maxSummaryCandidates                     = 20
	maxSummaryStructuredRunes                = 16000
	maxSummaryArtifactBytes                  = 40000
	maxSummaryCandidateRunes                 = 2000
)

// SessionSummary is an immutable structured checkpoint for a profile generation.
type SessionSummary struct {
	ID                                                     int64
	UserID, SessionID                                      string
	SessionGeneration                                      int
	CoveredFromTurnID, CoveredThroughTurnID                int64
	Narrative                                              string
	OpenTasks, Commitments, Entities, Decisions, TopicTags []string
	SourceTurnIDs                                          []int64
}

// SessionPromptPressure becomes planner-visible only after successful delivery.
type SessionPromptPressure struct {
	Tokens, Limit int
	Version       string
}

// SummaryArtifact retains the approved version-1 receipt's JSON representation.
type SummaryArtifact struct {
	Narrative        string                        `json:"narrative"`
	OpenTasks        []string                      `json:"open_tasks"`
	Commitments      []string                      `json:"commitments"`
	Entities         []string                      `json:"entities"`
	Decisions        []string                      `json:"decisions"`
	TopicTags        []string                      `json:"topic_tags"`
	GenerationModel  string                        `json:"generation_model"`
	GeneratorVersion string                        `json:"generator_version"`
	Candidates       []CompactionCandidateArtifact `json:"candidates"`
}

// CompactionCandidateArtifact preserves the nested artifact wire shape. Active
// compression requires an empty candidates array; no fact publication exists.
type CompactionCandidateArtifact struct {
	SourceTurnID int64   `json:"source_turn_id"`
	Statement    string  `json:"statement"`
	Evidence     string  `json:"evidence"`
	Scope        string  `json:"scope"`
	Category     string  `json:"category"`
	Context      string  `json:"context"`
	Provenance   string  `json:"provenance"`
	Sensitivity  string  `json:"sensitivity"`
	Confidence   float64 `json:"confidence"`
	Importance   int     `json:"importance"`
	TTLDays      int     `json:"ttl_days"`
	Supersedes   string  `json:"supersedes"`
	ClaimSlot    string  `json:"claim_slot"`
	ClaimValue   string  `json:"claim_value"`
}

// ActiveSessionScope identifies an active profile conversation generation.
type ActiveSessionScope struct {
	UserID, SessionID string
	Generation        int
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
