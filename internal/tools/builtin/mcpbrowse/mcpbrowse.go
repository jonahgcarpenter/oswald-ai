package mcpbrowse

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// NewServersHandler returns a handler that lists configured MCP servers.
func NewServersHandler(manager *mcpmanager.Manager, log *config.Logger) registry.Handler {
	return func(ctx context.Context, arguments map[string]interface{}) (string, error) {
		meta := requestctx.MetadataFromContext(ctx)
		reqLog := log.Agent("agent.tool.mcp.servers", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model)
		servers := manager.ServerInfos()
		reqLog.Debug("agent.tool.mcp.servers", "listed MCP servers", config.F("server_count", len(servers)))
		if len(servers) == 0 {
			return "No MCP servers are configured.", nil
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Configured MCP servers:\n")
		for i, server := range servers {
			fmt.Fprintf(&b, "%d. %s\n", i+1, server.Name)
			fmt.Fprintf(&b, "Status: %s\n", server.Status)
			fmt.Fprintf(&b, "Tools: %d\n", server.ToolCount)
			if server.Description != "" {
				fmt.Fprintf(&b, "Description: %s\n", server.Description)
			}
			if server.Reason != "" {
				fmt.Fprintf(&b, "Reason: %s\n", server.Reason)
			}
			if i < len(servers)-1 {
				fmt.Fprintf(&b, "\n")
			}
		}
		return strings.TrimSpace(b.String()), nil
	}
}

// NewToolsHandler returns a handler that lists and exposes MCP tools for this request.
func NewToolsHandler(reg *registry.Registry, manager *mcpmanager.Manager, log *config.Logger) registry.Handler {
	return func(ctx context.Context, arguments map[string]interface{}) (string, error) {
		server := strings.TrimSpace(stringArg(arguments, "server"))
		if server == "" {
			return "", fmt.Errorf("server is required")
		}
		query := strings.TrimSpace(stringArg(arguments, "query"))
		limit := intArg(arguments, "limit", 8)
		serverInfo, ok := manager.ServerInfo(server)
		if !ok {
			return fmt.Sprintf("No configured MCP server named %q. Use mcp.servers to list configured servers.", server), nil
		}
		if serverInfo.Status != "connected" {
			if serverInfo.Reason != "" {
				return fmt.Sprintf("MCP server %q is configured but unavailable: %s", serverInfo.Name, serverInfo.Reason), nil
			}
			return fmt.Sprintf("MCP server %q is configured but unavailable.", serverInfo.Name), nil
		}

		tools := searchTools(reg, serverInfo.Name, query, limit)
		if len(tools) == 0 {
			if query != "" {
				return fmt.Sprintf("No MCP tools matched server %q for query %q.", serverInfo.Name, query), nil
			}
			return fmt.Sprintf("No MCP tools matched server %q.", serverInfo.Name), nil
		}

		names := make([]string, 0, len(tools))
		for _, tool := range tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		if exposer := requestctx.ToolExposerFromContext(ctx); exposer != nil {
			exposer.ExposeTools(names)
		}

		meta := requestctx.MetadataFromContext(ctx)
		reqLog := log.Agent("agent.tool.mcp.tools", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model)
		reqLog.Debug("agent.tool.mcp.tools", "listed MCP tools", config.F("server", serverInfo.Name), config.F("query", query), config.F("tool_count", len(tools)))

		var b strings.Builder
		fmt.Fprintf(&b, "Available MCP tools from %s", serverInfo.Name)
		if query != "" {
			fmt.Fprintf(&b, " matching %q", query)
		}
		fmt.Fprintf(&b, ":\n")
		for i, tool := range tools {
			fmt.Fprintf(&b, "%d. %s\n", i+1, tool.Name)
			fmt.Fprintf(&b, "Server: %s\n", tool.Server)
			if tool.Description != "" {
				fmt.Fprintf(&b, "Description: %s\n", tool.Description)
			}
			required := requiredParams(tool.Parameters)
			if len(required) > 0 {
				fmt.Fprintf(&b, "Required parameters: %s\n", strings.Join(required, ", "))
			}
			if i < len(tools)-1 {
				fmt.Fprintf(&b, "\n")
			}
		}
		fmt.Fprintf(&b, "\n\nThese tools are now available for direct tool calls in this request. Tool descriptions and parameters are provided by the MCP server.")
		return strings.TrimSpace(b.String()), nil
	}
}

func searchTools(reg *registry.Registry, server, query string, limit int) []registry.CatalogEntry {
	server = strings.TrimSpace(strings.ToLower(server))
	query = strings.TrimSpace(strings.ToLower(query))
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}

	type scoredEntry struct {
		entry registry.CatalogEntry
		score int
	}
	var matches []scoredEntry
	for _, tool := range reg.CatalogBySource(registry.ToolSourceMCP) {
		if server != "" && strings.ToLower(tool.Server) != server {
			continue
		}
		score := toolSearchScore(tool, query)
		if query != "" && score == 0 {
			continue
		}
		matches = append(matches, scoredEntry{entry: tool, score: score})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].entry.Name < matches[j].entry.Name
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}
	tools := make([]registry.CatalogEntry, 0, len(matches))
	for _, match := range matches {
		tools = append(tools, match.entry)
	}
	return tools
}

func toolSearchScore(tool registry.CatalogEntry, query string) int {
	if query == "" {
		return 1
	}
	score := 0
	name := strings.ToLower(tool.Name)
	server := strings.ToLower(tool.Server)
	description := strings.ToLower(tool.Description)
	if strings.Contains(name, query) {
		score += 100
	}
	if strings.Contains(server, query) {
		score += 50
	}
	if strings.Contains(description, query) {
		score += 25
	}
	for _, term := range strings.Fields(query) {
		if strings.Contains(name, term) {
			score += 20
		}
		if strings.Contains(description, term) {
			score += 10
		}
		for _, param := range tool.Parameters {
			if strings.Contains(strings.ToLower(param.Name), term) {
				score += 8
			}
			if strings.Contains(strings.ToLower(param.Description), term) {
				score += 4
			}
		}
	}
	return score
}

func requiredParams(params []registry.ParamSpec) []string {
	var required []string
	for _, param := range params {
		if param.Required {
			required = append(required, param.Name)
		}
	}
	sort.Strings(required)
	return required
}

func stringArg(args map[string]interface{}, name string) string {
	value, _ := args[name].(string)
	return value
}

func intArg(args map[string]interface{}, name string, fallback int) int {
	switch value := args[name].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case float32:
		return int(value)
	default:
		return fallback
	}
}
