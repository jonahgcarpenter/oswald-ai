package budget

const (
	defaultContextWindow = 32768
	defaultSafetyMargin  = 256
)

// CompactionTriggerPercent is the input-pressure threshold for foreground and durable compaction.
const CompactionTriggerPercent = 70

// TrimThresholdPercent is the severe-pressure threshold for large->small model
// swaps (issue #295). Normal 70-100% pressure still summarizes (no data loss);
// only over-budget prompts (which steady-state compaction at 70% can never
// produce — only a window shrink can) drop oldest turns unsummarized to the
// 70% line first, then the remainder compacts normally on the session model.
// Dropped turns stay searchable via session_search.
const TrimThresholdPercent = 100

// KeepTokens is the verbatim recent tail kept beside the compaction summary
// (opencode keep.tokens parity). Newest turns fitting this token budget stay
// in context; older turns are summarization candidates. Flat across models:
// modern windows dwarf it, and the per-model 70% trigger still bounds total
// pressure.
const KeepTokens = 15000

// ContextBudget describes the request-time prompt budget derived from the
// active model's context window.
type ContextBudget struct {
	ContextWindow int
	SafetyMargin  int
	PromptLimit   int
}

// UsableInputLimit returns the model input capacity after the safety margin.
// An explicit prompt limit can further constrain that capacity.
func (b ContextBudget) UsableInputLimit() int {
	limit := b.PromptLimit
	if b.ContextWindow > 0 {
		contextLimit := b.ContextWindow
		if limit <= 0 || contextLimit < limit {
			limit = contextLimit
		}
	}
	limit -= b.SafetyMargin
	if limit < 0 {
		return 0
	}
	return limit
}

// NewContextBudget derives prompt-budget settings from the configured context window.
// A non-positive window uses the package default; no output capacity is reserved.
func NewContextBudget(contextWindow int) ContextBudget {
	budget := ContextBudget{
		ContextWindow: defaultContextWindow,
		SafetyMargin:  defaultSafetyMargin,
	}
	if contextWindow > 0 {
		budget.ContextWindow = contextWindow
	}
	return budget
}
