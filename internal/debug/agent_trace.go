package debug

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// TraceToolParameter describes one tool parameter in the trace output.
type TraceToolParameter struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// TraceTool contains one tool definition in the trace output.
type TraceTool struct {
	Name        string
	Description string
	Parameters  []TraceToolParameter
}

// TraceMCPServer groups traced MCP tools under a single server name.
type TraceMCPServer struct {
	Server string
	Tools  []TraceTool
}

// SemanticMemoryTrace records vector-memory behavior for one agent request.
type SemanticMemoryTrace struct {
	Enabled                 bool
	EmbeddingModel          string
	RecentTurnLimit         int
	IncludeRecent           bool
	MaxRelevantTurnLimit    int
	MinSimilarity           float64
	QueryAttempted          bool
	QueryHadReplyContext    bool
	QueryStatus             string
	QueryDurationMS         int64
	QueryEmbeddingDimension int
	CandidateTurnCount      int
	SelectedTurnCount       int
	RecentTurnCount         int
	SemanticTurnCount       int
	QueryError              string
	StoreAttempted          bool
	StoreStatus             string
	StoreDurationMS         int64
	StoreEmbeddingDimension int
	StoreError              string
	Selections              []SemanticMemorySelectionTrace
}

// SemanticMemorySelectionTrace records why a retained turn was selected or skipped.
type SemanticMemorySelectionTrace struct {
	Index          int
	CreatedAt      time.Time
	UserChars      int
	AssistantChars int
	Similarity     float64
	HasSimilarity  bool
	Included       bool
	Reason         string
}

// AgentTrace contains the final trace payload for a single agent request.
type AgentTrace struct {
	Dir                        string
	SessionKey                 string
	Model                      string
	Messages                   []ollama.ChatMessage
	BuiltinTools               []TraceTool
	MCPServers                 []TraceMCPServer
	ContextWindow              int
	PromptBudget               int
	EstimatedBefore            int
	EstimatedAfter             int
	RemovedPairs               int
	RequestImages              []ollama.InputImage
	SemanticMemory             SemanticMemoryTrace
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
	fmt.Fprintf(&sb, "| Tools | %d |\n", totalTraceTools(trace))
	fmt.Fprintf(&sb, "| Tool executions | %d |\n", len(trace.ToolExecutions))
	fmt.Fprintf(&sb, "| Context window | %d tokens |\n", trace.ContextWindow)
	fmt.Fprintf(&sb, "| Prompt budget | %d tokens |\n", trace.PromptBudget)
	fmt.Fprintf(&sb, "| Estimated tokens (before pruning) | %d |\n", trace.EstimatedBefore)
	fmt.Fprintf(&sb, "| Estimated tokens (after pruning) | %d |\n", trace.EstimatedAfter)
	fmt.Fprintf(&sb, "| Turn pairs compacted by budget pressure | %d |\n", trace.RemovedPairs)
	fmt.Fprintf(&sb, "| Tool failure budget exhausted | %t |\n", trace.ToolFailureBudgetExhausted)
	fmt.Fprintf(&sb, "| Current request images | %d |\n", len(trace.RequestImages))
	sb.WriteString("\n---\n\n")

	if trace.SemanticMemory.Enabled {
		writeSemanticMemoryTrace(&sb, trace.SemanticMemory)
	}

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

	if len(trace.BuiltinTools) > 0 {
		fmt.Fprintf(&sb, "## Builtin Tools (%d)\n\n", len(trace.BuiltinTools))
		for _, tool := range trace.BuiltinTools {
			writeTraceTool(&sb, tool)
		}
	}

	mcpToolCount := 0
	for _, server := range trace.MCPServers {
		mcpToolCount += len(server.Tools)
	}
	if mcpToolCount > 0 {
		fmt.Fprintf(&sb, "## MCP Tools (%d)\n\n", mcpToolCount)
		for _, server := range trace.MCPServers {
			fmt.Fprintf(&sb, "### Server: `%s`\n\n", server.Server)
			for _, tool := range server.Tools {
				writeTraceTool(&sb, tool)
			}
		}
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("debug: failed to write %q: %w", path, err)
	}

	return nil
}

func writeSemanticMemoryTrace(sb *strings.Builder, trace SemanticMemoryTrace) {
	sb.WriteString("## Semantic Memory\n\n")
	sb.WriteString("| Field | Value |\n")
	sb.WriteString("|---|---|\n")
	fmt.Fprintf(sb, "| Enabled | %t |\n", trace.Enabled)
	fmt.Fprintf(sb, "| Embedding model | `%s` |\n", trace.EmbeddingModel)
	fmt.Fprintf(sb, "| Recent turn limit | %d |\n", trace.RecentTurnLimit)
	fmt.Fprintf(sb, "| Include recent automatically | %t |\n", trace.IncludeRecent)
	fmt.Fprintf(sb, "| Max relevant turn limit | %d |\n", trace.MaxRelevantTurnLimit)
	fmt.Fprintf(sb, "| Min similarity | %.2f |\n", trace.MinSimilarity)
	fmt.Fprintf(sb, "| Query attempted | %t |\n", trace.QueryAttempted)
	fmt.Fprintf(sb, "| Query stripped reply context | %t |\n", trace.QueryHadReplyContext)
	fmt.Fprintf(sb, "| Query status | `%s` |\n", trace.QueryStatus)
	fmt.Fprintf(sb, "| Query duration | %d ms |\n", trace.QueryDurationMS)
	fmt.Fprintf(sb, "| Query embedding dimensions | %d |\n", trace.QueryEmbeddingDimension)
	fmt.Fprintf(sb, "| Candidate turns | %d |\n", trace.CandidateTurnCount)
	fmt.Fprintf(sb, "| Selected turns | %d |\n", trace.SelectedTurnCount)
	fmt.Fprintf(sb, "| Recent turns selected | %d |\n", trace.RecentTurnCount)
	fmt.Fprintf(sb, "| Semantic turns selected | %d |\n", trace.SemanticTurnCount)
	if trace.QueryError != "" {
		fmt.Fprintf(sb, "| Query error | `%s` |\n", trace.QueryError)
	}
	fmt.Fprintf(sb, "| Store attempted | %t |\n", trace.StoreAttempted)
	fmt.Fprintf(sb, "| Store status | `%s` |\n", trace.StoreStatus)
	fmt.Fprintf(sb, "| Store duration | %d ms |\n", trace.StoreDurationMS)
	fmt.Fprintf(sb, "| Store embedding dimensions | %d |\n", trace.StoreEmbeddingDimension)
	if trace.StoreError != "" {
		fmt.Fprintf(sb, "| Store error | `%s` |\n", trace.StoreError)
	}
	if len(trace.Selections) > 0 {
		sb.WriteString("\n### Candidate Selection\n\n")
		sb.WriteString("| Turn | Created | User chars | Assistant chars | Similarity | Included | Reason |\n")
		sb.WriteString("|---|---|---:|---:|---:|---|---|\n")
		for _, selection := range trace.Selections {
			similarity := "n/a"
			if selection.HasSimilarity {
				similarity = fmt.Sprintf("%.3f", selection.Similarity)
			}
			reason := selection.Reason
			if reason == "" {
				reason = "skipped"
			}
			fmt.Fprintf(sb, "| %d | %s | %d | %d | %s | %t | `%s` |\n",
				selection.Index+1,
				selection.CreatedAt.UTC().Format(time.RFC3339),
				selection.UserChars,
				selection.AssistantChars,
				similarity,
				selection.Included,
				reason,
			)
		}
	}
	sb.WriteString("\n---\n\n")
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

func totalTraceTools(trace AgentTrace) int {
	total := len(trace.BuiltinTools)
	for _, server := range trace.MCPServers {
		total += len(server.Tools)
	}
	return total
}

func writeTraceTool(sb *strings.Builder, tool TraceTool) {
	fmt.Fprintf(sb, "### `%s`\n\n", tool.Name)
	fmt.Fprintf(sb, "%s\n\n", tool.Description)
	if len(tool.Parameters) == 0 {
		return
	}

	params := append([]TraceToolParameter(nil), tool.Parameters...)
	sort.Slice(params, func(i, j int) bool {
		return params[i].Name < params[j].Name
	})

	sb.WriteString("| Parameter | Type | Required | Description |\n")
	sb.WriteString("|---|---|---|---|\n")
	for _, param := range params {
		required := "no"
		if param.Required {
			required = "yes"
		}
		fmt.Fprintf(sb, "| `%s` | %s | %s | %s |\n", param.Name, param.Type, required, markdownTableEscape(param.Description))
	}
	sb.WriteString("\n")
}
