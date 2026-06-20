package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	githubmcp "github.com/jonahgcarpenter/oswald-ai/internal/mcp/github"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Manager owns the configured MCP client sessions for the lifetime of the app.
type Manager struct {
	servers []*server
	log     *config.Logger
}

// NewManagerFromConfig initializes all eager MCP servers enabled by config.
func NewManagerFromConfig(ctx context.Context, cfg *config.Config, log *config.Logger) (*Manager, error) {
	manager := &Manager{log: log.Server("mcp.manager")}
	if !cfg.GitHubMCPEnabled() {
		manager.log.Info("mcp.bootstrap.disabled", "github MCP disabled", config.F("server", "github"), config.F("reason", "missing_token"), config.F("status", "degraded"))
		return manager, nil
	}

	githubServer, err := newGitHubServer(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	manager.servers = append(manager.servers, githubServer)
	manager.log.Info("mcp.bootstrap.enabled", "enabled MCP servers", config.F("server_count", len(manager.servers)), config.F("servers", manager.ServerNames()))
	return manager, nil
}

// ToolSpecs returns all discovered MCP tools across enabled servers.
func (m *Manager) ToolSpecs() []ToolSpec {
	out := make([]ToolSpec, 0)
	for _, srv := range m.servers {
		out = append(out, srv.tools...)
	}
	return out
}

// ServerCount returns the number of connected MCP servers.
func (m *Manager) ServerCount() int {
	if m == nil {
		return 0
	}
	return len(m.servers)
}

// ServerNames returns the enabled MCP server names.
func (m *Manager) ServerNames() string {
	if m == nil || len(m.servers) == 0 {
		return ""
	}
	names := make([]string, 0, len(m.servers))
	for _, srv := range m.servers {
		names = append(names, srv.name)
	}
	return strings.Join(names, ",")
}

// Close shuts down all connected MCP sessions.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}

	var errs []error
	for _, srv := range m.servers {
		if srv.close == nil {
			continue
		}
		if err := srv.close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s MCP session: %w", srv.name, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func newGitHubServer(ctx context.Context, cfg *config.Config, log *config.Logger) (*server, error) {
	provider, err := githubmcp.Connect(ctx, cfg, log)
	if err != nil {
		return nil, fmt.Errorf("connect github MCP server: %w", err)
	}

	tools, err := loadGitHubToolSpecs(ctx, provider.Session(), log)
	if err != nil {
		provider.Close() // nolint: errcheck
		return nil, err
	}

	log.Server("mcp.github").Info("mcp.server.connect.complete", "connected MCP server", config.F("server", "github"), config.F("tool_count", len(tools)), config.F("status", "ok"))
	return &server{name: "github", tools: tools, close: provider.Close}, nil
}

func loadGitHubToolSpecs(ctx context.Context, session *gomcp.ClientSession, log *config.Logger) ([]ToolSpec, error) {
	serverLog := log.Server("mcp.github")
	var specs []ToolSpec
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &gomcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list github MCP tools: %w", err)
		}

		for _, tool := range result.Tools {
			if tool == nil || !githubmcp.IsReadOnlyTool(tool) {
				continue
			}
			spec, err := githubToolSpec(tool, session, log)
			if err != nil {
				serverLog.Warn("mcp.tool.skipped", "skipped MCP tool", config.F("server", "github"), config.F("tool_name", tool.Name), config.F("status", "degraded"), config.ErrorField(err))
				continue
			}
			specs = append(specs, spec)
		}

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	return specs, nil
}

func githubToolSpec(tool *gomcp.Tool, session *gomcp.ClientSession, log *config.Logger) (ToolSpec, error) {
	params, err := schemaToParams(tool.InputSchema)
	if err != nil {
		return ToolSpec{}, fmt.Errorf("normalize input schema: %w", err)
	}

	toolName := tool.Name
	localName := "github." + toolName
	description := strings.TrimSpace(tool.Description)
	if description == "" {
		description = strings.TrimSpace(tool.Title)
	}

	return ToolSpec{
		Name:        localName,
		Description: description,
		Server:      "github",
		Parameters:  params,
		Handler: func(ctx context.Context, arguments map[string]interface{}) (string, error) {
			meta := requestctx.MetadataFromContext(ctx)
			reqLog := log.Agent("agent.tool.mcp.github", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model)
			reqLog.Debug("agent.tool.mcp.start", "starting MCP tool execution", config.F("tool_name", localName), config.F("remote_tool_name", toolName), config.F("server", "github"))

			result, err := session.CallTool(ctx, &gomcp.CallToolParams{Name: toolName, Arguments: arguments})
			if err != nil {
				return "", fmt.Errorf("github MCP tool %q failed: %w", toolName, err)
			}

			flattened, err := flattenToolResult(result)
			if err != nil {
				return "", fmt.Errorf("format github MCP tool %q result: %w", toolName, err)
			}
			return flattened, nil
		},
	}, nil
}
