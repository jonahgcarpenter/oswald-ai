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
			content := call.Result
			if call.Name == "vision_analyze" {
				content = "[Historical image tool result; image bytes are omitted and are not loaded in this turn. Visibility and delivery statements below describe the original turn. Use vision_analyze with an available image source if needed.]\n\n" + content
			}
			messages = append(messages, llm.ChatMessage{Role: "tool", ToolName: call.Name, ToolCallID: callID, Content: content})
		}
	}
	return append(messages, llm.ChatMessage{Role: "assistant", Content: turn.AssistantText})
}
