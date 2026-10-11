package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/exposure"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// MCPProvider resolves request-scoped MCP tools for the active canonical user.
type MCPProvider interface {
	DiscoveryTools(ctx context.Context, principal identity.Principal) []llm.Tool
	ResolveTools(ctx context.Context, principal identity.Principal, names []string) []string
	LLMTools(ctx context.Context, principal identity.Principal, exposed map[string]bool) []llm.Tool
	Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposed map[string]bool) (mcp.ExecutionResult, bool, error)
	ToolPolicy(name string) governance.ToolPolicy
}

type offeredToolCatalog struct {
	Tools    []llm.Tool
	Policies map[string]governance.ToolPolicy
}

func (a *Agent) toolsForRequest(ctx context.Context, principal identity.Principal, exposure *exposure.Exposure, governor *governance.Governor) offeredToolCatalog {
	catalog := offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy)}
	add := func(tool llm.Tool, policy governance.ToolPolicy) {
		name := tool.Function.Name
		if _, exists := catalog.Policies[name]; name == "" || exists {
			return
		}
		if governor != nil && governor.IsToolRetired(name, policy) {
			return
		}
		catalog.Tools = append(catalog.Tools, tool)
		catalog.Policies[name] = policy
	}
	for _, tool := range a.registry.LLMToolsForVisibility(exposure.Visibility()) {
		if policy, ok := a.registry.Policy(tool.Function.Name); ok {
			add(tool, policy)
		}
	}
	if a.mcpProvider == nil {
		return catalog
	}
	for _, tool := range a.mcpProvider.DiscoveryTools(ctx, principal) {
		add(tool, a.mcpProvider.ToolPolicy(tool.Function.Name))
	}
	for _, tool := range a.mcpProvider.LLMTools(ctx, principal, exposure.ExposedMCPTools()) {
		add(tool, a.mcpProvider.ToolPolicy(tool.Function.Name))
	}
	return catalog
}

// toolsForStatelessRequest combines caller-supplied client tools with Oswald
// tools for stateless API requests. Client tools are never executed
// server-side; when the model calls one, the agent returns it verbatim for
// the caller to run. Oswald tool names always win on collision.
//
// Per OpenAI spec norm, Oswald builtins are opt-in: ""/"auto" offers only the
// caller's tools, "none" offers nothing, "required" needs a nonempty caller
// catalog, "named" offers exactly the forced tool (builtin or client), and
// "allowed" intersects the merged catalog with the caller's allow-list.
// Unknown names and an empty "required" catalog fail with an error the caller
// maps to a 400.
func (a *Agent) toolsForStatelessRequest(base offeredToolCatalog, clientTools []llm.Tool, choice ClientToolChoice) (offeredToolCatalog, error) {
	clientOnly := offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy, len(clientTools))}
	for _, tool := range clientTools {
		name := tool.Function.Name
		if name == "" {
			continue
		}
		if _, exists := clientOnly.Policies[name]; exists {
			continue
		}
		if _, exists := base.Policies[name]; exists {
			continue
		}
		if a.registry != nil && a.registry.HasHandler(name) {
			continue
		}
		clientOnly.Tools = append(clientOnly.Tools, tool)
		clientOnly.Policies[name] = governance.ToolPolicy{}
	}
	merged := offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy, len(base.Policies)+len(clientOnly.Policies))}
	merged.Tools = append(merged.Tools, base.Tools...)
	for name, policy := range base.Policies {
		merged.Policies[name] = policy
	}
	merged.Tools = append(merged.Tools, clientOnly.Tools...)
	for name, policy := range clientOnly.Policies {
		merged.Policies[name] = policy
	}
	switch choice.Mode {
	case "", "auto":
		return clientOnly, nil
	case "none":
		return offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy)}, nil
	case "required":
		if len(clientOnly.Tools) == 0 {
			return offeredToolCatalog{}, errors.New("no_tools_available")
		}
		return clientOnly, nil
	case "named":
		for _, tool := range merged.Tools {
			if tool.Function.Name == choice.Named {
				filtered := offeredToolCatalog{Policies: map[string]governance.ToolPolicy{choice.Named: merged.Policies[choice.Named]}}
				filtered.Tools = append(filtered.Tools, tool)
				return filtered, nil
			}
		}
		return offeredToolCatalog{}, errors.New("named_tool_missing")
	case "allowed":
		if len(choice.Allowed) == 0 {
			return offeredToolCatalog{}, errors.New("named_tool_missing")
		}
		allowed := make(map[string]bool, len(choice.Allowed))
		for _, name := range choice.Allowed {
			allowed[name] = true
		}
		filtered := offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy, len(choice.Allowed))}
		for _, tool := range merged.Tools {
			name := tool.Function.Name
			if !allowed[name] {
				continue
			}
			filtered.Tools = append(filtered.Tools, tool)
			filtered.Policies[name] = merged.Policies[name]
		}
		if len(filtered.Tools) != len(choice.Allowed) {
			return offeredToolCatalog{}, errors.New("named_tool_missing")
		}
		return filtered, nil
	default:
		return offeredToolCatalog{}, errors.New("named_tool_missing")
	}
}

// isClientToolCall reports whether a tool name refers to a caller-owned tool
// with no server-side handler.
func (a *Agent) isClientToolCall(clientTools []llm.Tool, name string) bool {
	if a.registry != nil && a.registry.HasHandler(name) {
		return false
	}
	for _, tool := range clientTools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func (a *Agent) executeTool(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}, exposure *exposure.Exposure) (governance.Result, error) {
	if a.registry.HasHandler(name) {
		return a.registry.Execute(ctx, name, args)
	}
	if a.mcpProvider != nil {
		if result, handled, err := a.mcpProvider.Execute(ctx, principal, name, args, exposure.ExposedMCPTools()); handled {
			return result.Result, err
		}
	}
	return a.registry.Execute(ctx, name, args)
}

func normalizeToolCallIDs(message *llm.ChatMessage, iteration int) {
	if message == nil {
		return
	}
	reserved := make(map[string]bool, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if id := strings.TrimSpace(call.ID); id != "" {
			reserved[id] = true
		}
	}
	used := make(map[string]bool, len(message.ToolCalls))
	for i := range message.ToolCalls {
		id := strings.TrimSpace(message.ToolCalls[i].ID)
		if id != "" && !used[id] {
			message.ToolCalls[i].ID = id
			used[id] = true
			continue
		}
		base := fmt.Sprintf("call_%d_%d", iteration, i+1)
		id = base
		for suffix := 2; reserved[id] || used[id]; suffix++ {
			id = fmt.Sprintf("%s_%d", base, suffix)
		}
		message.ToolCalls[i].ID = id
		used[id] = true
	}
}

func governanceResultText(reason string) string {
	switch reason {
	case governance.ReasonDuplicate:
		return "Tool call blocked: the same tool and arguments were already executed in this request. Use the existing result or try meaningfully different arguments."
	case governance.ReasonToolLimit:
		return "Tool call blocked: this tool reached its execution limit for the request. Continue with the available results."
	case governance.ReasonToolFailures:
		return "Tool call blocked: the tool failure limit was reached. Continue without retrying this tool."
	case governance.ReasonToolUnproductive:
		return "Tool call blocked: this tool returned too many unproductive results. Continue with the available information."
	case governance.ReasonGlobalLimit, governance.ReasonIterationLimit:
		return "Tool call blocked: the request tool budget was exhausted. Finish the answer using the available results."
	case governance.ReasonUnadvertised:
		return "Tool call blocked: this tool was not available for this model step. Use only currently available tools."
	default:
		return "Tool call blocked by request policy. Continue with the available information."
	}
}
