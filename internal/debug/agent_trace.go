package debug

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// ToolExecutionTrace records one raw tool execution during an agent request.
type ToolExecutionTrace struct {
	Iteration int
	Name      string
	Arguments map[string]interface{}
	Result    string
	Error     string
}

// AgentTrace contains the final trace payload for a single agent request.
type AgentTrace struct {
	Dir                        string
	SessionKey                 string
	Model                      string
	Messages                   []ollama.ChatMessage
	Tools                      []ollama.Tool
	ContextWindow              int
	PromptBudget               int
	EstimatedBefore            int
	EstimatedAfter             int
	RemovedPairs               int
	RequestImages              []ollama.InputImage
	ToolExecutions             []ToolExecutionTrace
	FinalResponse              string
	FinalThinking              string
	FinalModelResponse         *ollama.ChatResponse
	ToolFailureBudgetExhausted bool
}

// DumpAgentTrace writes the full agent trace for a single request to a
// timestamped Markdown file under trace.Dir.
func DumpAgentTrace(trace AgentTrace) error {
	if trace.Dir == "" {
		return nil
	}

	if err := os.MkdirAll(trace.Dir, 0o755); err != nil {
		return fmt.Errorf("debug: failed to create directory %q: %w", trace.Dir, err)
	}

	ts := time.Now().UTC()
	safeSession := SanitizeFilePart(trace.SessionKey, 16)
	filename := fmt.Sprintf("trace_%s_%s.md", safeSession, ts.Format("20060102_150405"))
	path := filepath.Join(trace.Dir, filename)

	var sb strings.Builder

	fmt.Fprintf(&sb, "# Agent Trace Dump\n\n")
	fmt.Fprintf(&sb, "| Field | Value |\n")
	fmt.Fprintf(&sb, "|---|---|\n")
	fmt.Fprintf(&sb, "| Generated | %s |\n", ts.Format(time.RFC1123))
	fmt.Fprintf(&sb, "| Session | `%s` |\n", trace.SessionKey)
	fmt.Fprintf(&sb, "| Model | `%s` |\n", trace.Model)
	fmt.Fprintf(&sb, "| Messages | %d |\n", len(trace.Messages))
	fmt.Fprintf(&sb, "| Tools | %d |\n", len(trace.Tools))
	fmt.Fprintf(&sb, "| Tool executions | %d |\n", len(trace.ToolExecutions))
	fmt.Fprintf(&sb, "| Context window | %d tokens |\n", trace.ContextWindow)
	fmt.Fprintf(&sb, "| Prompt budget | %d tokens |\n", trace.PromptBudget)
	fmt.Fprintf(&sb, "| Estimated tokens (before pruning) | %d |\n", trace.EstimatedBefore)
	fmt.Fprintf(&sb, "| Estimated tokens (after pruning) | %d |\n", trace.EstimatedAfter)
	fmt.Fprintf(&sb, "| Turn pairs compacted by budget pressure | %d |\n", trace.RemovedPairs)
	fmt.Fprintf(&sb, "| Tool failure budget exhausted | %t |\n", trace.ToolFailureBudgetExhausted)
	fmt.Fprintf(&sb, "| Current request images | %d |\n", len(trace.RequestImages))
	sb.WriteString("\n---\n\n")

	fmt.Fprintf(&sb, "## Full Message Transcript (%d messages)\n\n", len(trace.Messages))
	pendingToolCalls := make([]ollama.ToolCall, 0)
	for i, msg := range trace.Messages {
		fmt.Fprintf(&sb, "### [%d] role: `%s`", i+1, msg.Role)
		if msg.ToolName != "" {
			fmt.Fprintf(&sb, " · tool: `%s`", msg.ToolName)
		}
		sb.WriteString("\n\n")

		contentDuplicatesThinking := msg.Content != "" && msg.Thinking != "" && msg.Content == msg.Thinking
		isFinalModelResponse := i == len(trace.Messages)-1 && msg.Role == "assistant"

		if len(msg.Images) > 0 {
			fmt.Fprintf(&sb, "**Images:** %d attached\n\n", len(msg.Images))
		}

		if msg.Thinking != "" {
			sb.WriteString("**Thinking:**\n\n```\n")
			sb.WriteString(msg.Thinking)
			if !strings.HasSuffix(msg.Thinking, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n\n")
		}

		if len(msg.ToolCalls) > 0 {
			pendingToolCalls = append(pendingToolCalls, msg.ToolCalls...)
		}

		if msg.Role == "tool" && len(pendingToolCalls) > 0 {
			toolCallIndex := -1
			for idx, tc := range pendingToolCalls {
				if msg.ToolName == "" || tc.Function.Name == msg.ToolName {
					toolCallIndex = idx
					break
				}
			}
			if toolCallIndex >= 0 {
				tc := pendingToolCalls[toolCallIndex]
				pendingToolCalls = append(pendingToolCalls[:toolCallIndex], pendingToolCalls[toolCallIndex+1:]...)
				sb.WriteString("**Raw tool calls:**\n\n")
				argsJSON, _ := json.MarshalIndent(tc.Function.Arguments, "", "  ")
				fmt.Fprintf(&sb, "- `%s`\n\n```json\n%s\n```\n\n", tc.Function.Name, argsJSON)
			}
		}

		if msg.Content != "" && !contentDuplicatesThinking {
			if isFinalModelResponse {
				sb.WriteString("**Reponse:**\n\n")
			} else if msg.Role == "tool" {
				sb.WriteString("**Results:**\n\n")
			}
			sb.WriteString("```\n")
			sb.WriteString(msg.Content)
			if !strings.HasSuffix(msg.Content, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n\n")
		}

		sb.WriteString("---\n\n")
	}

	if len(trace.RequestImages) > 0 {
		fmt.Fprintf(&sb, "## Request Images (%d)\n\n", len(trace.RequestImages))
		for i, image := range trace.RequestImages {
			fmt.Fprintf(&sb, "- [%d] mime=`%s` source=`%s`\n", i+1, image.MimeType, image.Source)
		}
		sb.WriteString("\n")
	}

	if trace.FinalModelResponse != nil {
		resp := trace.FinalModelResponse
		tokensPerSecond := 0.0
		if resp.EvalDuration > 0 {
			tokensPerSecond = float64(resp.EvalCount) / (float64(resp.EvalDuration) / 1e9)
		}
		sb.WriteString("## Model Metrics\n\n")
		sb.WriteString("| Field | Value |\n")
		sb.WriteString("|---|---|\n")
		fmt.Fprintf(&sb, "| Model | `%s` |\n", resp.Model)
		fmt.Fprintf(&sb, "| Total duration | %d ms |\n", resp.TotalDuration/1e6)
		fmt.Fprintf(&sb, "| Load duration | %d ms |\n", resp.LoadDuration/1e6)
		fmt.Fprintf(&sb, "| Prompt eval duration | %d ms |\n", resp.PromptEvalDuration/1e6)
		fmt.Fprintf(&sb, "| Eval duration | %d ms |\n", resp.EvalDuration/1e6)
		fmt.Fprintf(&sb, "| Prompt eval count | %d |\n", resp.PromptEvalCount)
		fmt.Fprintf(&sb, "| Eval count | %d |\n", resp.EvalCount)
		fmt.Fprintf(&sb, "| Tokens per second | %.2f |\n\n", tokensPerSecond)
	}

	if len(trace.Tools) > 0 {
		fmt.Fprintf(&sb, "## Available Tools (%d)\n\n", len(trace.Tools))
		for _, t := range trace.Tools {
			fmt.Fprintf(&sb, "### `%s`\n\n", t.Function.Name)
			fmt.Fprintf(&sb, "%s\n\n", t.Function.Description)
			if len(t.Function.Parameters.Properties) > 0 {
				sb.WriteString("| Parameter | Type | Required | Description |\n")
				sb.WriteString("|---|---|---|---|\n")
				for name, prop := range t.Function.Parameters.Properties {
					required := "no"
					for _, req := range t.Function.Parameters.Required {
						if req == name {
							required = "yes"
							break
						}
					}
					fmt.Fprintf(&sb, "| `%s` | %s | %s | %s |\n", name, prop.Type, required, markdownTableEscape(prop.Description))
				}
				sb.WriteString("\n")
			}
		}
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("debug: failed to write %q: %w", path, err)
	}

	return nil
}

// SanitizeFilePart returns at most maxLen runes of s with any character that
// is not a letter, digit, hyphen, or underscore replaced by an underscore.
func SanitizeFilePart(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		runes = runes[:maxLen]
	}
	for i, r := range runes {
		if !IsFileSafe(r) {
			runes[i] = '_'
		}
	}
	return string(runes)
}

// IsFileSafe reports whether r is safe to include in a filename.
func IsFileSafe(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_'
}

func markdownTableEscape(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.TrimSpace(s)
}
