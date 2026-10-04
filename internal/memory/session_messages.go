package memory

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// SessionTurnMessages renders a complete exchange with correlated native tools.
func SessionTurnMessages(turn SessionTurn) []llm.ChatMessage {
	messages := []llm.ChatMessage{{Role: "user", Content: turn.UserText}}
	for batchIndex, batch := range turn.ToolHistory.Batches {
		assistant := llm.ChatMessage{Role: "assistant", Content: batch.AssistantContent}
		for callIndex, call := range batch.Calls {
			callID := fmt.Sprintf("hist_%d_%d_%d", turn.ID, batchIndex+1, callIndex+1)
			assistant.ToolCalls = append(assistant.ToolCalls, llm.ToolCall{ID: callID, Function: llm.ToolFunction{Name: call.Name, Arguments: call.Arguments}})
		}
		messages = append(messages, assistant)
		for callIndex, call := range batch.Calls {
			callID := fmt.Sprintf("hist_%d_%d_%d", turn.ID, batchIndex+1, callIndex+1)
			messages = append(messages, llm.ChatMessage{Role: "tool", ToolName: call.Name, ToolCallID: callID, Content: call.Result})
		}
	}
	return append(messages, llm.ChatMessage{Role: "assistant", Content: turn.AssistantText})
}
