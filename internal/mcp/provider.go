package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Provider exposes scoped MCP tools to the agent for a single request.
type Provider struct {
	manager *Manager
}

func NewProvider(manager *Manager) *Provider {
	return &Provider{manager: manager}
}

func (p *Provider) LLMTools(ctx context.Context, userID string, exposed map[string]bool) []llm.Tool {
	if p == nil || p.manager == nil || len(exposed) == 0 {
		return nil
	}
	specs := p.manager.ToolSpecs(ctx, userID)
	tools := make([]llm.Tool, 0, len(specs))
	for _, spec := range specs {
		if !exposed[spec.Name] {
			continue
		}
		tools = append(tools, llmTool(spec))
	}
	return tools
}

func (p *Provider) Execute(ctx context.Context, userID, name string, args map[string]interface{}) (string, bool, error) {
	if p == nil || p.manager == nil || !strings.Contains(name, ".") {
		return "", false, nil
	}
	server, _, ok := strings.Cut(name, ".")
	if !ok {
		return "", false, nil
	}
	if _, visible := p.manager.ServerInfo(ctx, userID, server); !visible {
		return "", false, nil
	}
	result, err := p.manager.Execute(ctx, userID, name, args)
	return result, true, err
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
