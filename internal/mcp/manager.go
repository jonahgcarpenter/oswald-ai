package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Manager owns scoped MCP client sessions and resolves tools for active users.
type Manager struct {
	store    *Store
	sessions map[string]*server
	mu       sync.Mutex
	log      *config.Logger
}

// NewManagerFromStore creates a DB-backed MCP manager.
func NewManagerFromStore(store *Store, log *config.Logger) *Manager {
	return &Manager{store: store, sessions: make(map[string]*server), log: log.Server("mcp.manager")}
}

// ServerInfos returns global and user-scoped MCP server metadata visible to userID.
func (m *Manager) ServerInfos(ctx context.Context, userID string) []ServerInfo {
	if m == nil || m.store == nil {
		return nil
	}
	configs, err := m.store.ListForUser(ctx, userID)
	if err != nil {
		m.log.Warn("mcp.server_configs.list_failed", "failed to list MCP servers", config.F("status", "degraded"), config.ErrorField(err))
		return nil
	}
	infos := make([]ServerInfo, 0, len(configs))
	for _, cfg := range configs {
		info := ServerInfo{Name: cfg.Name, Scope: cfg.Scope, OwnerUserID: cfg.OwnerUserID, Status: serverStatusNotConnected}
		if !cfg.Enabled {
			info.Status = serverStatusDisabled
			infos = append(infos, info)
			continue
		}
		if srv := m.cached(scopeKey(cfg)); srv != nil {
			if srv.reason != "" {
				info.Status = serverStatusError
				info.Reason = srv.reason
			} else {
				info.Status = serverStatusConnected
				info.ToolCount = len(srv.tools)
			}
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Scope != infos[j].Scope {
			return infos[i].Scope < infos[j].Scope
		}
		return infos[i].Name < infos[j].Name
	})
	return infos
}

// ServerInfo returns a visible server by name for the active user.
func (m *Manager) ServerInfo(ctx context.Context, userID string, name string) (ServerInfo, bool) {
	name = strings.TrimSpace(strings.ToLower(name))
	for _, info := range m.ServerInfos(ctx, userID) {
		if info.Name == name {
			return info, true
		}
	}
	return ServerInfo{}, false
}

// ToolSpecs returns currently connected tools visible to userID.
func (m *Manager) ToolSpecs(ctx context.Context, userID string) []ToolSpec {
	configs, err := m.store.ListForUser(ctx, userID)
	if err != nil {
		return nil
	}
	var specs []ToolSpec
	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		srv, err := m.ensureConnected(ctx, cfg)
		if err != nil {
			m.log.Warn("mcp.server.connect_failed", "failed to connect MCP server", config.F("server", cfg.Name), config.F("scope", cfg.Scope), config.F("status", "degraded"), config.ErrorField(err))
			continue
		}
		specs = append(specs, srv.tools...)
	}
	return specs
}

// ServerToolSpecs returns tools for a single visible server, connecting lazily.
func (m *Manager) ServerToolSpecs(ctx context.Context, userID, name string) ([]ToolSpec, ServerInfo, error) {
	cfg, ok, err := m.resolveConfig(ctx, userID, name)
	if err != nil {
		return nil, ServerInfo{}, err
	}
	if !ok {
		return nil, ServerInfo{}, fmt.Errorf("no configured MCP server named %q", name)
	}
	info := ServerInfo{Name: cfg.Name, Scope: cfg.Scope, OwnerUserID: cfg.OwnerUserID, Status: serverStatusNotConnected}
	if !cfg.Enabled {
		info.Status = serverStatusDisabled
		return nil, info, nil
	}
	srv, err := m.ensureConnected(ctx, cfg)
	if err != nil {
		info.Status = serverStatusError
		info.Reason = config.SafeErrorText(err)
		return nil, info, nil
	}
	info.Status = serverStatusConnected
	info.ToolCount = len(srv.tools)
	return append([]ToolSpec(nil), srv.tools...), info, nil
}

// Execute calls a scoped MCP tool visible to userID.
func (m *Manager) Execute(ctx context.Context, userID string, toolName string, args map[string]interface{}) (string, error) {
	serverName, remoteName, ok := splitToolName(toolName)
	if !ok {
		return "", fmt.Errorf("invalid MCP tool name %q", toolName)
	}
	tols, _, err := m.ServerToolSpecs(ctx, userID, serverName)
	if err != nil {
		return "", err
	}
	for _, tool := range tols {
		if tool.RemoteName == remoteName || tool.Name == toolName {
			return tool.Handler(ctx, args)
		}
	}
	return "", fmt.Errorf("MCP tool %q is not available", toolName)
}

// Close shuts down connected MCP sessions.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for key, srv := range m.sessions {
		if srv.close != nil {
			if err := srv.close(); err != nil {
				errs = append(errs, fmt.Errorf("close %s MCP session: %w", key, err))
			}
		}
		delete(m.sessions, key)
	}
	return errors.Join(errs...)
}

// Invalidate closes any cached session for a server whose config changed.
func (m *Manager) Invalidate(scope, ownerUserID, name string) {
	if m == nil {
		return
	}
	key := scope + ":" + strings.TrimSpace(name)
	if scope == ScopeUser {
		key = scope + ":" + strings.TrimSpace(ownerUserID) + ":" + strings.TrimSpace(name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if srv := m.sessions[key]; srv != nil && srv.close != nil {
		srv.close() // nolint:errcheck
	}
	delete(m.sessions, key)
}

func (m *Manager) cached(key string) *server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[key]
}

func (m *Manager) resolveConfig(ctx context.Context, userID, name string) (ServerConfig, bool, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if cfg, ok, err := m.store.Get(ctx, ScopeGlobal, "", name); err != nil || ok {
		return cfg, ok, err
	}
	if strings.TrimSpace(userID) == "" {
		return ServerConfig{}, false, nil
	}
	return m.store.Get(ctx, ScopeUser, userID, name)
}

func (m *Manager) ensureConnected(ctx context.Context, cfg ServerConfig) (*server, error) {
	key := scopeKey(cfg)
	if srv := m.cached(key); srv != nil && srv.reason == "" {
		return srv, nil
	}
	if _, err := parseAndValidateURL(ctx, cfg.URL, m.store.resolver); err != nil {
		m.rememberError(key, cfg, err)
		return nil, err
	}
	if cfg.Transport != TransportStreamableHTTP {
		err := fmt.Errorf("MCP transport %q is not implemented", cfg.Transport)
		m.rememberError(key, cfg, err)
		return nil, err
	}
	session, closeFn, err := connectStreamableHTTP(ctx, cfg)
	if err != nil {
		m.rememberError(key, cfg, err)
		return nil, err
	}
	tools, err := loadToolSpecs(ctx, cfg, session, m.log)
	if err != nil {
		closeFn() // nolint:errcheck
		m.rememberError(key, cfg, err)
		return nil, err
	}
	srv := &server{config: cfg, tools: tools, close: closeFn}
	m.mu.Lock()
	if old := m.sessions[key]; old != nil && old.close != nil {
		old.close() // nolint:errcheck
	}
	m.sessions[key] = srv
	m.mu.Unlock()
	m.log.Info("mcp.server.connect.complete", "connected MCP server", config.F("server", cfg.Name), config.F("scope", cfg.Scope), config.F("tool_count", len(tools)), config.F("status", "ok"))
	return srv, nil
}

func (m *Manager) rememberError(key string, cfg ServerConfig, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[key] = &server{config: cfg, reason: config.SafeErrorText(err)}
}

func connectStreamableHTTP(ctx context.Context, cfg ServerConfig) (*gomcp.ClientSession, func() error, error) {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &headerTransport{
			base:    http.DefaultTransport,
			headers: cfg.Headers,
		},
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "oswald-ai", Version: "1.0.0"}, &gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}})
	transport := &gomcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient, DisableStandaloneSSE: true}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect MCP session: %w", err)
	}
	return session, session.Close, nil
}

func loadToolSpecs(ctx context.Context, cfg ServerConfig, session *gomcp.ClientSession, log *config.Logger) ([]ToolSpec, error) {
	var specs []ToolSpec
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &gomcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list MCP tools: %w", err)
		}
		for _, tool := range result.Tools {
			if tool == nil {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(tool.Name), "tools") {
				log.Warn("mcp.tool.skipped", "skipped MCP tool with reserved name", config.F("server", cfg.Name), config.F("tool_name", tool.Name), config.F("status", "degraded"))
				continue
			}
			spec, err := toolSpec(cfg, tool, session, log)
			if err != nil {
				log.Warn("mcp.tool.skipped", "skipped MCP tool", config.F("server", cfg.Name), config.F("tool_name", tool.Name), config.F("status", "degraded"), config.ErrorField(err))
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

func toolSpec(cfg ServerConfig, tool *gomcp.Tool, session *gomcp.ClientSession, log *config.Logger) (ToolSpec, error) {
	params, err := schemaToParams(tool.InputSchema)
	if err != nil {
		return ToolSpec{}, fmt.Errorf("normalize input schema: %w", err)
	}
	remoteName := strings.TrimSpace(tool.Name)
	localName := cfg.Name + "." + remoteName
	description := strings.TrimSpace(tool.Description)
	if description == "" {
		description = strings.TrimSpace(tool.Title)
	}
	return ToolSpec{Name: localName, Description: description, Server: cfg.Name, Scope: cfg.Scope, OwnerUserID: cfg.OwnerUserID, RemoteName: remoteName, Parameters: params, Handler: func(ctx context.Context, arguments map[string]interface{}) (string, error) {
		meta := requestctx.MetadataFromContext(ctx)
		reqLog := log.Agent("agent.tool.mcp", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model)
		reqLog.Debug("agent.tool.mcp.start", "starting MCP tool execution", config.F("tool_name", localName), config.F("remote_tool_name", remoteName), config.F("server", cfg.Name), config.F("scope", cfg.Scope))
		result, err := session.CallTool(ctx, &gomcp.CallToolParams{Name: remoteName, Arguments: arguments})
		if err != nil {
			return "", fmt.Errorf("MCP tool %q failed: %w", remoteName, err)
		}
		flattened, err := flattenToolResult(result)
		if err != nil {
			return "", fmt.Errorf("format MCP tool %q result: %w", remoteName, err)
		}
		return flattened, nil
	}}, nil
}

func scopeKey(cfg ServerConfig) string {
	if cfg.Scope == ScopeGlobal {
		return ScopeGlobal + ":" + cfg.Name
	}
	return ScopeUser + ":" + cfg.OwnerUserID + ":" + cfg.Name
}

func splitToolName(name string) (string, string, bool) {
	server, remote, ok := strings.Cut(strings.TrimSpace(name), ".")
	return server, remote, ok && server != "" && remote != ""
}
