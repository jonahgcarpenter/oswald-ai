package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxToolResultChars = 16000

func flattenToolResult(result *gomcp.CallToolResult) (string, error) {
	if result == nil {
		return "", fmt.Errorf("empty MCP tool result")
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
				return "", fmt.Errorf("marshal MCP content: %w", err)
			}
			parts = append(parts, string(data))
		}
	}

	if len(parts) == 0 && result.StructuredContent != nil {
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return "", fmt.Errorf("marshal structured content: %w", err)
		}
		parts = append(parts, string(data))
	}

	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if result.IsError {
		if text == "" {
			text = "MCP tool returned an unspecified error."
		}
		text = "Error: " + text
	}
	if text == "" {
		text = "MCP tool returned no content."
	}

	return truncate(text, maxToolResultChars), nil
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n\n[truncated]"
}
