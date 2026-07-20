package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Provider exposes scoped MCP tools to the agent for a single request.
type Provider struct {
	manager *Manager
}

func NewProvider(manager *Manager) *Provider {
	return &Provider{manager: manager}
}

func (p *Provider) DiscoveryTools(ctx context.Context, principal identity.Principal) []llm.Tool {
	if p == nil || p.manager == nil || p.manager.store == nil || !principal.Valid() {
		return nil
	}
	configs, err := p.manager.store.ListForUser(ctx, principal.CanonicalUserID)
	if err != nil {
		return nil
	}
	tools := make([]llm.Tool, 0, len(configs))
	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		tools = append(tools, discoveryTool(cfg.Name))
	}
	return tools
}

// ResolveTools returns historical MCP tool names that remain available to the principal.
func (p *Provider) ResolveTools(ctx context.Context, principal identity.Principal, names []string) []string {
	if p == nil || p.manager == nil || len(names) == 0 || !principal.Valid() {
		return nil
	}

	servers := make([]string, 0)
	seenServers := map[string]bool{}
	candidates := make([]string, 0, len(names))
	seenCandidates := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		server, remote, ok := splitToolName(name)
		if !ok || strings.EqualFold(remote, "tools") || seenCandidates[name] {
			continue
		}
		seenCandidates[name] = true
		candidates = append(candidates, name)
		if !seenServers[server] {
			seenServers[server] = true
			servers = append(servers, server)
		}
	}

	available := map[string]bool{}
	for _, server := range servers {
		specs, info, err := p.manager.ServerToolSpecs(ctx, principal.CanonicalUserID, server)
		if err != nil || info.Status != serverStatusConnected {
			continue
		}
		for _, spec := range specs {
			available[spec.Name] = true
		}
	}

	resolved := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if available[name] {
			resolved = append(resolved, name)
		}
	}
	return resolved
}

func (p *Provider) LLMTools(ctx context.Context, principal identity.Principal, exposed map[string]bool) []llm.Tool {
	if p == nil || p.manager == nil || len(exposed) == 0 || !principal.Valid() {
		return nil
	}
	specs := p.manager.ToolSpecs(ctx, principal.CanonicalUserID)
	tools := make([]llm.Tool, 0, len(specs))
	for _, spec := range specs {
		if !exposed[spec.Name] {
			continue
		}
		tools = append(tools, llmTool(spec))
	}
	return tools
}

func (p *Provider) Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposed map[string]bool) (ExecutionResult, bool, error) {
	if p == nil || p.manager == nil || !principal.Valid() || !strings.Contains(name, ".") {
		return ExecutionResult{}, false, nil
	}
	server, remote, ok := strings.Cut(name, ".")
	if !ok {
		return ExecutionResult{}, false, nil
	}
	if remote == "tools" {
		result, err := p.discover(ctx, principal, server, args)
		return ExecutionResult{Content: result, ServerName: server, IsDiscovery: true}, true, err
	}
	if !exposed[name] {
		return ExecutionResult{}, false, nil
	}
	if _, visible := p.manager.ServerInfo(ctx, principal.CanonicalUserID, server); !visible {
		return ExecutionResult{}, false, nil
	}
	result, err := p.manager.Execute(ctx, principal.CanonicalUserID, name, args)
	return result, true, err
}

func discoveryTool(server string) llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolDefinition{
		Name:        server + ".tools",
		Description: "Search and expose MCP tools from the configured " + server + " server for this request. Call this before using any " + server + ".* MCP tool.",
		Parameters: llm.ToolParameters{
			Type: "object",
			Properties: map[string]llm.ToolParameterProperty{
				"query": {Type: "string", Description: "Optional case-insensitive search text. Matches tool names, descriptions, and parameter names/descriptions. Omit to return all tools from this server."},
			},
		},
	}}
}

func (p *Provider) discover(ctx context.Context, principal identity.Principal, server string, args map[string]interface{}) (string, error) {
	query := strings.TrimSpace(stringArg(args, "query"))
	entries, info, err := p.Catalog(ctx, principal.CanonicalUserID, server)
	if err != nil {
		return fmt.Sprintf("No configured MCP server named %q.", server), nil
	}
	if info.Status != serverStatusConnected {
		if info.Reason != "" {
			return fmt.Sprintf("MCP server %q is configured but unavailable: %s", info.Name, info.Reason), nil
		}
		return fmt.Sprintf("MCP server %q is configured but unavailable.", info.Name), nil
	}
	tools := searchTools(entries, info.Name, query)
	if len(tools) == 0 {
		if query != "" {
			return fmt.Sprintf("No MCP tools matched server %q for query %q.", info.Name, query), nil
		}
		return fmt.Sprintf("No MCP tools matched server %q.", info.Name), nil
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
	if p.manager.log != nil {
		reqLog := p.manager.log.Agent("agent.tool.mcp.discovery", meta.RequestID, meta.SessionID, principal.CanonicalUserID, principal.Gateway, meta.Model)
		reqLog.Debug("agent.tool.mcp.discovery", "listed MCP tools", config.F("server", info.Name), config.F("query", query), config.F("tool_count", len(tools)))
	}
	return formatDiscoveryResult(info.Name, query, tools), nil
}

func (p *Provider) Catalog(ctx context.Context, userID string, server string) ([]registry.CatalogEntry, ServerInfo, error) {
	specs, info, err := p.manager.ServerToolSpecs(ctx, userID, server)
	if err != nil {
		return nil, info, err
	}
	entries := make([]registry.CatalogEntry, 0, len(specs))
	for _, spec := range specs {
		entries = append(entries, registry.CatalogEntry{Name: spec.Name, Description: spec.Description, Source: registry.ToolSourceMCP, Server: spec.Server, Parameters: mapParams(spec.Parameters)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, info, nil
}

func llmTool(spec ToolSpec) llm.Tool {
	props := make(map[string]llm.ToolParameterProperty, len(spec.Parameters))
	required := []string{}
	for _, p := range spec.Parameters {
		props[p.Name] = llm.ToolParameterProperty{Type: p.Type, Description: p.Description, Enum: p.Enum}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	description := strings.TrimSpace(spec.Description)
	if description == "" {
		description = "MCP tool from " + spec.Server
	} else {
		description = "MCP tool from " + spec.Server + ": " + description
	}
	return llm.Tool{Type: "function", Function: llm.ToolDefinition{Name: spec.Name, Description: description, Parameters: llm.ToolParameters{Type: "object", Properties: props, Required: required}}}
}

func mapParams(params []ParamSpec) []registry.ParamSpec {
	out := make([]registry.ParamSpec, 0, len(params))
	for _, p := range params {
		out = append(out, registry.ParamSpec{Name: p.Name, Type: p.Type, Required: p.Required, Description: p.Description, Enum: p.Enum})
	}
	return out
}

func searchTools(catalog []registry.CatalogEntry, server, query string) []registry.CatalogEntry {
	server = strings.TrimSpace(strings.ToLower(server))
	query = strings.TrimSpace(strings.ToLower(query))
	type scoredEntry struct {
		entry registry.CatalogEntry
		score int
	}
	var matches []scoredEntry
	for _, tool := range catalog {
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

func formatDiscoveryResult(server, query string, tools []registry.CatalogEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Available MCP tools from %s", server)
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
	return strings.TrimSpace(b.String())
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
