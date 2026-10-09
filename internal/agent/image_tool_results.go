package agent

import (
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	imagegenerate "github.com/jonahgcarpenter/oswald-ai/internal/tools/image_generate"
	visionanalyze "github.com/jonahgcarpenter/oswald-ai/internal/tools/vision_analyze"
)

const maxToolResultImages = 4

func boundImageToolResults(messages []llm.ChatMessage) []llm.ChatMessage {
	result := append([]llm.ChatMessage(nil), messages...)
	remaining := map[string]int{visionanalyze.Name: maxToolResultImages, imagegenerate.Name: maxToolResultImages}
	for i := len(result) - 1; i >= 0; i-- {
		message := &result[i]
		if message.Role != "tool" || (message.ToolName != visionanalyze.Name && message.ToolName != imagegenerate.Name) || len(message.Images) == 0 {
			continue
		}
		if remaining[message.ToolName] > 0 {
			remaining[message.ToolName] -= len(message.Images)
			continue
		}
		message.Images = nil
		message.Content += "\n\nNote: This preview is no longer included in active context; only the latest four images returned by this tool are retained."
	}
	return result
}

// retainedImageToolRounds preserves whole correlated assistant/tool batches
// containing active image inputs when foreground compaction rebuilds context.
func retainedImageToolRounds(messages []llm.ChatMessage) []llm.ChatMessage {
	var retained []llm.ChatMessage
	for i := 0; i < len(messages); i++ {
		assistant := messages[i]
		if assistant.Role != "assistant" || len(assistant.ToolCalls) == 0 {
			continue
		}
		end := i + 1
		hasImage := false
		for end < len(messages) && messages[end].Role == "tool" {
			message := messages[end]
			hasImage = hasImage || ((message.ToolName == visionanalyze.Name || message.ToolName == imagegenerate.Name) && len(message.Images) > 0)
			end++
		}
		if !hasImage || end-i-1 != len(assistant.ToolCalls) {
			continue
		}
		pending := make(map[string]string, len(assistant.ToolCalls))
		for _, call := range assistant.ToolCalls {
			pending[call.ID] = call.Function.Name
		}
		complete := len(pending) == len(assistant.ToolCalls)
		for _, result := range messages[i+1 : end] {
			name, found := pending[result.ToolCallID]
			complete = complete && found && name == result.ToolName
			delete(pending, result.ToolCallID)
		}
		if !complete || len(pending) != 0 {
			continue
		}
		assistant.Thinking = ""
		assistant.ToolCalls = append([]llm.ToolCall(nil), assistant.ToolCalls...)
		for j := range assistant.ToolCalls {
			call := &assistant.ToolCalls[j]
			if call.Function.Name == visionanalyze.Name || call.Function.Name == imagegenerate.Name {
				call.Function.Arguments = imageCheckpointArguments(call.Function.Arguments)
				call.Function.RawArguments = ""
			}
		}
		retained = append(retained, assistant)
		retained = append(retained, messages[i+1:end]...)
		i = end - 1
	}
	return retained
}

// imageCheckpointArguments excludes inline attachment bytes from compaction,
// including invalid image_generate data URL arguments in a retained mixed batch.
func imageCheckpointArguments(args map[string]interface{}) map[string]interface{} {
	source, _ := args["image_url"].(string)
	if !strings.HasPrefix(source, "data:") {
		return args
	}
	copy := make(map[string]interface{}, len(args))
	for key, value := range args {
		copy[key] = value
	}
	copy["image_url"] = "[Inline image data omitted]"
	return copy
}
