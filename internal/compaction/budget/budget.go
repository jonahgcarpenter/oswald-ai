package budget

const (
	defaultContextWindow = 32768
	defaultSafetyMargin  = 256
)

// CompactionTriggerPercent is the input-pressure threshold for foreground and durable compaction.
const CompactionTriggerPercent = 70

// RecentTailLimit reserves 25 percent of input for recent exchanges, bounded to
// 2,000 through 8,000 tokens without exceeding the available input.
func RecentTailLimit(inputLimit int) int {
	limit := inputLimit / 4
	if limit < 2000 {
		limit = 2000
	}
	if limit > 8000 {
		limit = 8000
	}
	if limit > inputLimit {
		limit = inputLimit
	}
	return limit
}

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
