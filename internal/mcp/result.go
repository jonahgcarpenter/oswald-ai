package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxToolResultChars = 16000

func flattenToolResult(result *gomcp.CallToolResult) (governance.Result, error) {
	if result == nil {
		return governance.Result{}, fmt.Errorf("MCP response was invalid (category: result_format). Do not retry unchanged")
	}

	parts := make([]string, 0, len(result.Content)+1)
	for _, content := range result.Content {
		switch c := content.(type) {
		case *gomcp.TextContent:
			text := strings.TrimSpace(c.Text)
			if text != "" {
				parts = append(parts, text)
			}
		case *gomcp.ResourceLink:
			name := strings.TrimSpace(c.Title)
			if name == "" {
				name = strings.TrimSpace(c.Name)
			}
			if name == "" {
				name = c.URI
			}
			parts = append(parts, fmt.Sprintf("[resource] %s\nURI: %s", name, c.URI))
		default:
			data, err := json.Marshal(c)
			if err != nil {
				return governance.Result{}, fmt.Errorf("MCP response was invalid (category: result_format). Do not retry unchanged")
			}
			parts = append(parts, string(data))
		}
	}

	if len(parts) == 0 && result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return governance.Result{}, fmt.Errorf("MCP response was invalid (category: result_format). Do not retry unchanged")
		}
		parts = append(parts, string(data))
	}

	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if result.IsError {
		return governance.Result{}, fmt.Errorf("MCP tool reported failure (category: remote_tool). Review the arguments before retrying")
	}
	outcome := governance.OutcomeProductive
	reason := ""
	if text == "" {
		text = "MCP tool returned no content."
		outcome = governance.OutcomeUnproductive
		reason = "no_content"
	}

	return governance.Result{Content: governance.BoundResultText(text, maxToolResultChars), Outcome: outcome, ReasonCode: reason}, nil
}
