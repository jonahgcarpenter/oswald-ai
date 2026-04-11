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

// DumpPrompt writes the full prompt context for a single request to a
// timestamped Markdown file under dir.
func DumpPrompt(
	dir string,
	sessionKey string,
	model string,
	messages []ollama.ChatMessage,
	tools []ollama.Tool,
	contextWindow int,
	promptBudget int,
	estimatedBefore int,
	estimatedAfter int,
	removedPairs int,
	requestImages []ollama.InputImage,
) error {
	if dir == "" {
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("debug: failed to create directory %q: %w", dir, err)
	}

	ts := time.Now().UTC()
	safeSession := SanitizeFilePart(sessionKey, 16)
	filename := fmt.Sprintf("prompt_%s_%s.md", safeSession, ts.Format("20060102_150405"))
	path := filepath.Join(dir, filename)

	var sb strings.Builder

	fmt.Fprintf(&sb, "# Prompt Debug Dump\n\n")
	fmt.Fprintf(&sb, "| Field | Value |\n")
	fmt.Fprintf(&sb, "|---|---|\n")
	fmt.Fprintf(&sb, "| Generated | %s |\n", ts.Format(time.RFC1123))
	fmt.Fprintf(&sb, "| Session | `%s` |\n", sessionKey)
	fmt.Fprintf(&sb, "| Model | `%s` |\n", model)
	fmt.Fprintf(&sb, "| Messages | %d |\n", len(messages))
	fmt.Fprintf(&sb, "| Tools | %d |\n", len(tools))
	fmt.Fprintf(&sb, "| Context window | %d tokens |\n", contextWindow)
	fmt.Fprintf(&sb, "| Prompt budget | %d tokens |\n", promptBudget)
	fmt.Fprintf(&sb, "| Estimated tokens (before pruning) | %d |\n", estimatedBefore)
	fmt.Fprintf(&sb, "| Estimated tokens (after pruning) | %d |\n", estimatedAfter)
	fmt.Fprintf(&sb, "| Turn pairs compacted by budget pressure | %d |\n", removedPairs)
	fmt.Fprintf(&sb, "| Current request images | %d |\n", len(requestImages))
	sb.WriteString("\n---\n\n")
	fmt.Fprintf(&sb, "## Actual Request Sent to Ollama (%d messages)\n\n", len(messages))
	for i, msg := range messages {
		fmt.Fprintf(&sb, "### [%d] role: `%s`", i+1, msg.Role)
		if msg.ToolName != "" {
			fmt.Fprintf(&sb, " · tool: `%s`", msg.ToolName)
		}
		sb.WriteString("\n\n")

		if msg.Content != "" {
			sb.WriteString("```\n")
			sb.WriteString(msg.Content)
			if !strings.HasSuffix(msg.Content, "\n") {
				sb.WriteString("\n")
			}
			sb.WriteString("```\n\n")
		}

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
			sb.WriteString("**Tool calls:**\n\n")
			for _, tc := range msg.ToolCalls {
				argsJSON, _ := json.MarshalIndent(tc.Function.Arguments, "", "  ")
				fmt.Fprintf(&sb, "- `%s`\n\n```json\n%s\n```\n\n", tc.Function.Name, argsJSON)
			}
		}

		sb.WriteString("---\n\n")
	}

	if len(requestImages) > 0 {
		fmt.Fprintf(&sb, "## Request Images (%d)\n\n", len(requestImages))
		for i, image := range requestImages {
			fmt.Fprintf(&sb, "- [%d] mime=`%s` source=`%s`\n", i+1, image.MimeType, image.Source)
		}
		sb.WriteString("\n")
	}

	if len(tools) > 0 {
		fmt.Fprintf(&sb, "## Tools (%d)\n\n", len(tools))
		for _, t := range tools {
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
