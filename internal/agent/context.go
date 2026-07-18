package agent

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/promptbudget"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// PromptContext is a role-correct model context assembled within an input
// token limit. SelectedTurns are returned in chronological message order.
type PromptContext struct {
	Messages           []llm.ChatMessage
	SelectedTurns      []usermemory.SessionTurn
	SelectedToolNames  []string
	SelectedTurnCount  int
	OmittedTurnCount   int
	RequiredEstimate   int
	EstimatedBefore    int
	EstimatedAfter     int
	InputLimit         int
	RequiredOverBudget bool
}

// AssemblePromptContext builds model messages from required request content
// and a newest-first list of completed session turns. Historical exchanges are
// included only as whole user/assistant pairs and are emitted chronologically.
func AssemblePromptContext(
	deploymentPolicy string,
	tenantProfile string,
	currentPrompt string,
	currentImages []llm.InputImage,
	recentTurns []usermemory.SessionTurn,
	tools []llm.Tool,
	inputLimit int,
) PromptContext {
	required := make([]llm.ChatMessage, 0, 3)
	required = append(required, llm.ChatMessage{Role: "system", Content: deploymentPolicy})
	if tenantProfile != "" {
		required = append(required, llm.ChatMessage{Role: "user", Content: tenantProfile})
	}
	current := llm.ChatMessage{
		Role:    "user",
		Content: currentPrompt,
		Images:  append([]llm.InputImage(nil), currentImages...),
	}
	required = append(required, current)

	result := PromptContext{
		InputLimit:       inputLimit,
		RequiredEstimate: promptbudget.EstimateRequest(required, tools),
	}
	allMessages := messagesWithTurns(required, recentTurns)
	result.EstimatedBefore = promptbudget.EstimateRequest(allMessages, tools)

	selectedNewestFirst := make([]usermemory.SessionTurn, 0, len(recentTurns))
	for _, turn := range recentTurns {
		candidate := append(selectedNewestFirst, turn)
		candidateMessages := messagesWithTurns(required, candidate)
		if promptbudget.EstimateRequest(candidateMessages, tools) > inputLimit {
			break
		}
		selectedNewestFirst = candidate
	}

	result.SelectedTurns = reverseTurns(selectedNewestFirst)
	result.Messages = messagesWithChronologicalTurns(required, result.SelectedTurns)
	result.SelectedToolNames = selectedToolNames(result.SelectedTurns)
	result.SelectedTurnCount = len(result.SelectedTurns)
	result.OmittedTurnCount = len(recentTurns) - result.SelectedTurnCount
	result.EstimatedAfter = promptbudget.EstimateRequest(result.Messages, tools)
	result.RequiredOverBudget = result.RequiredEstimate > inputLimit
	return result
}

func messagesWithTurns(required []llm.ChatMessage, newestFirst []usermemory.SessionTurn) []llm.ChatMessage {
	return messagesWithChronologicalTurns(required, reverseTurns(newestFirst))
}

func messagesWithChronologicalTurns(required []llm.ChatMessage, chronological []usermemory.SessionTurn) []llm.ChatMessage {
	messages := make([]llm.ChatMessage, 0, len(required)+len(chronological)*2)
	last := len(required) - 1
	messages = append(messages, required[:last]...)
	for _, turn := range chronological {
		messages = append(messages,
			llm.ChatMessage{Role: "user", Content: turn.UserText},
			llm.ChatMessage{Role: "assistant", Content: assistantTurnContent(turn)},
		)
	}
	messages = append(messages, required[last])
	return messages
}

func assistantTurnContent(turn usermemory.SessionTurn) string {
	if len(turn.ToolNames) == 0 {
		return turn.AssistantText
	}
	return turn.AssistantText + "\n\nTools used: " + strings.Join(turn.ToolNames, ", ")
}

func reverseTurns(turns []usermemory.SessionTurn) []usermemory.SessionTurn {
	reversed := make([]usermemory.SessionTurn, len(turns))
	for i := range turns {
		reversed[len(turns)-1-i] = turns[i]
	}
	return reversed
}

func selectedToolNames(turns []usermemory.SessionTurn) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, turn := range turns {
		for _, name := range turn.ToolNames {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}

func uniqueToolNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
