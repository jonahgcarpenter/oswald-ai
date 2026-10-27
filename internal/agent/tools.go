package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/exposure"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/workspace"
)

// MCPProvider resolves request-scoped deferred MCP tools through the
// tool_search, tool_describe, and tool_call bridge.
type MCPProvider interface {
	BridgeTools(ctx context.Context, principal identity.Principal) []llm.Tool
	Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}) (mcp.ExecutionResult, bool, error)
	ToolPolicy(name string) governance.ToolPolicy
}

type offeredToolCatalog struct {
	Tools    []llm.Tool
	Policies map[string]governance.ToolPolicy
}

func (a *Agent) toolsForRequest(ctx context.Context, principal identity.Principal, exposure *exposure.Exposure, enabledToolsets []string) offeredToolCatalog {
	catalog := offeredToolCatalog{Policies: make(map[string]governance.ToolPolicy)}
	allowed := allowedBuiltins(enabledToolsets)
	add := func(tool llm.Tool, policy governance.ToolPolicy) {
		name := tool.Function.Name
		if _, exists := catalog.Policies[name]; name == "" || exists {
			return
		}
		if allowed != nil && !allowed[name] {
			return
		}
		catalog.Tools = append(catalog.Tools, tool)
		catalog.Policies[name] = policy
	}
	for _, tool := range a.registry.LLMToolsForVisibility(exposure.Visibility()) {
		if policy, ok := a.registry.Policy(tool.Function.Name); ok {
			tool.Function.Description = workspace.DynamicDescription(tool.Function.Name, tool.Function.Description)
			add(tool, policy)
		}
	}
	// The background-polling hint names process_manage, so it is injected
	// only when that tool is actually advertised; the static description
	// never references tools from other toolsets.
	if catalogHas(catalog, workspace.TerminalName) && catalogHas(catalog, workspace.ProcessManageName) {
		for i := range catalog.Tools {
			if catalog.Tools[i].Function.Name == workspace.TerminalName && !strings.Contains(catalog.Tools[i].Function.Description, "process_manage") {
				catalog.Tools[i].Function.Description += " Use background=true only for processes that must outlive the call, then poll with process_manage."
			}
		}
	}
	// A restricted toolset list suppresses the MCP bridge so scheduled runs
	// get deterministic catalogs; nil leaves interactive behavior unchanged.
	if enabledToolsets != nil || a.mcpProvider == nil {
		return catalog
	}
	for _, tool := range a.mcpProvider.BridgeTools(ctx, principal) {
		add(tool, a.mcpProvider.ToolPolicy(tool.Function.Name))
	}
	return catalog
}

// allowedBuiltins maps toolset names to their member tools. Nil means no
// filtering; an empty list hides every builtin.
func allowedBuiltins(enabledToolsets []string) map[string]bool {
	if enabledToolsets == nil {
		return nil
	}
	allowed := make(map[string]bool)
	for _, set := range enabledToolsets {
		for _, name := range tools.ToolsetMembers(set) {
			allowed[name] = true
		}
	}
	return allowed
}

func catalogHas(catalog offeredToolCatalog, name string) bool {
	_, ok := catalog.Policies[name]
	return ok
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
		if result, handled, err := a.mcpProvider.Execute(ctx, principal, name, args); handled {
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
	case governance.ReasonGlobalLimit, governance.ReasonIterationLimit:
		return "Tool call blocked: the request tool budget was exhausted. Finish the answer using the available results."
	case governance.ReasonUnadvertised:
		return "Tool call blocked: this tool was not available for this model step. Use only currently available tools."
	case governance.ReasonRepeatedExactBlock:
		return "Tool call blocked: the same tool call failed repeatedly with identical arguments. Stop retrying it unchanged; change strategy or explain the blocker."
	case governance.ReasonSameToolHalt, governance.ReasonIdenticalStreakHalt, governance.ReasonIdenticalCycleHalt:
		return "Tool call halted: the same failing tool path repeated without progress. Stop retrying it and choose a different approach."
	case governance.ReasonIdempotentNoProgressBlock:
		return "Tool call blocked: the same call returned the same result repeatedly. Use the earlier result or change the query."
	case governance.ReasonNoProgressWindowHalt:
		return "Tool call halted: the last several tool calls produced no productive result. Use the results already gathered or explain the blocker."
	default:
		return "Tool call blocked by request policy. Continue with the available information."
	}
}

// isProgressHaltReason reports whether a stop reason came from progress
// signals rather than the safety-valve count ceilings.
func isProgressHaltReason(reason string) bool {
	switch reason {
	case governance.ReasonRepeatedExactBlock,
		governance.ReasonSameToolHalt,
		governance.ReasonIdenticalStreakHalt,
		governance.ReasonIdenticalCycleHalt,
		governance.ReasonIdempotentNoProgressBlock,
		governance.ReasonNoProgressWindowHalt:
		return true
	default:
		return false
	}
}
