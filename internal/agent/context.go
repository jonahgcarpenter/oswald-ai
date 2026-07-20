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
	Messages            []llm.ChatMessage
	SelectedTurns       []usermemory.SessionTurn
	SelectedToolNames   []string
	SelectedTurnCount   int
	OmittedTurnCount    int
	SelectedRecallCount int
	OmittedRecallCount  int
	RecallChars         int
	SummaryIncluded     bool
	SummaryChars        int
	MinimumTailCount    int
	SelectedRecall      []usermemory.RecallResult
	RequiredEstimate    int
	EstimatedBefore     int
	EstimatedAfter      int
	InputLimit          int
	RequiredOverBudget  bool
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
	return AssemblePromptContextWithSummary(deploymentPolicy, tenantProfile, currentPrompt, currentImages, usermemory.SessionSummary{}, 0, nil, 0, recentTurns, tools, inputLimit)
}

// AssemblePromptContextWithRecall adds bounded durable recall to the current
// user message before selecting the newest complete session exchanges.
func AssemblePromptContextWithRecall(
	deploymentPolicy string,
	tenantProfile string,
	currentPrompt string,
	currentImages []llm.InputImage,
	recallResults []usermemory.RecallResult,
	recallCharLimit int,
	recentTurns []usermemory.SessionTurn,
	tools []llm.Tool,
	inputLimit int,
) PromptContext {
	return AssemblePromptContextWithSummary(deploymentPolicy, tenantProfile, currentPrompt, currentImages, usermemory.SessionSummary{}, 0, recallResults, recallCharLimit, recentTurns, tools, inputLimit)
}

// AssemblePromptContextWithSummary reserves a bounded historical summary and a
// minimum newest verbatim tail before recall and additional history.
func AssemblePromptContextWithSummary(
	deploymentPolicy string,
	tenantProfile string,
	currentPrompt string,
	currentImages []llm.InputImage,
	summary usermemory.SessionSummary,
	minimumTail int,
	recallResults []usermemory.RecallResult,
	recallCharLimit int,
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
		InputLimit:         inputLimit,
		RequiredEstimate:   promptbudget.EstimateRequest(required, tools),
		OmittedRecallCount: len(recallResults),
	}
	summaryBlock := usermemory.RenderSessionSummary(summary)
	allRequired := withRecall(required, usermemory.RenderDurableMemoryRecall(recallResults, recallCharLimit))
	allMessages := messagesWithSummaryAndTurns(allRequired, summaryBlock, recentTurns)
	result.EstimatedBefore = promptbudget.EstimateRequest(allMessages, tools)
	result.RequiredOverBudget = result.RequiredEstimate > inputLimit

	selectedNewestFirst := make([]usermemory.SessionTurn, 0, len(recentTurns))
	selectedSummary := ""
	if !result.RequiredOverBudget {
		if minimumTail < 0 {
			minimumTail = 0
		}
		if minimumTail > len(recentTurns) {
			minimumTail = len(recentTurns)
		}
		for _, turn := range recentTurns[:minimumTail] {
			candidate := append(selectedNewestFirst, turn)
			candidateMessages := messagesWithSummaryAndTurns(required, "", candidate)
			if promptbudget.EstimateRequest(candidateMessages, tools) > inputLimit {
				break
			}
			selectedNewestFirst = candidate
		}
		if summaryBlock != "" && promptbudget.EstimateRequest(messagesWithSummaryAndTurns(required, summaryBlock, selectedNewestFirst), tools) <= inputLimit {
			selectedSummary = summaryBlock
		}
	}
	result.MinimumTailCount = len(selectedNewestFirst)
	result.SummaryIncluded = selectedSummary != ""
	result.SummaryChars = len([]rune(selectedSummary))

	selectedRecall := make([]usermemory.RecallResult, 0, len(recallResults))
	if !result.RequiredOverBudget {
		for _, recall := range recallResults {
			candidate := append(selectedRecall, recall)
			block := usermemory.RenderDurableMemoryRecall(candidate, recallCharLimit)
			if block == "" || len(block) == len(usermemory.RenderDurableMemoryRecall(selectedRecall, recallCharLimit)) {
				continue
			}
			candidateRequired := withRecall(required, block)
			if promptbudget.EstimateRequest(messagesWithSummaryAndTurns(candidateRequired, selectedSummary, selectedNewestFirst), tools) > inputLimit {
				continue
			}
			selectedRecall = candidate
		}
	}
	recallBlock := usermemory.RenderDurableMemoryRecall(selectedRecall, recallCharLimit)
	required = withRecall(required, recallBlock)
	result.SelectedRecallCount = len(selectedRecall)
	result.SelectedRecall = append([]usermemory.RecallResult(nil), selectedRecall...)
	result.OmittedRecallCount = len(recallResults) - len(selectedRecall)
	result.RecallChars = len([]rune(recallBlock))

	if !result.RequiredOverBudget {
		for _, turn := range recentTurns[len(selectedNewestFirst):] {
			candidate := append(selectedNewestFirst, turn)
			if promptbudget.EstimateRequest(messagesWithSummaryAndTurns(required, selectedSummary, candidate), tools) > inputLimit {
				break
			}
			selectedNewestFirst = candidate
		}
	}

	result.SelectedTurns = reverseTurns(selectedNewestFirst)
	result.Messages = messagesWithSummaryAndChronologicalTurns(required, selectedSummary, result.SelectedTurns)
	result.SelectedToolNames = selectedToolNames(result.SelectedTurns)
	result.SelectedTurnCount = len(result.SelectedTurns)
	result.OmittedTurnCount = len(recentTurns) - result.SelectedTurnCount
	result.EstimatedAfter = promptbudget.EstimateRequest(result.Messages, tools)
	return result
}

func messagesWithSummaryAndTurns(required []llm.ChatMessage, summary string, newestFirst []usermemory.SessionTurn) []llm.ChatMessage {
	return messagesWithSummaryAndChronologicalTurns(required, summary, reverseTurns(newestFirst))
}

func messagesWithSummaryAndChronologicalTurns(required []llm.ChatMessage, summary string, chronological []usermemory.SessionTurn) []llm.ChatMessage {
	messages := make([]llm.ChatMessage, 0, len(required)+len(chronological)*2+1)
	last := len(required) - 1
	messages = append(messages, required[:last]...)
	if summary != "" {
		messages = append(messages, llm.ChatMessage{Role: "user", Content: summary})
	}
	for _, turn := range chronological {
		messages = append(messages,
			llm.ChatMessage{Role: "user", Content: turn.UserText},
			llm.ChatMessage{Role: "assistant", Content: assistantTurnContent(turn)},
		)
	}
	messages = append(messages, required[last])
	return messages
}

func withRecall(required []llm.ChatMessage, recallBlock string) []llm.ChatMessage {
	messages := append([]llm.ChatMessage(nil), required...)
	if recallBlock == "" {
		return messages
	}
	last := len(messages) - 1
	messages[last].Content = strings.TrimSpace(messages[last].Content + "\n\n" + recallBlock)
	return messages
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
