package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

// Bridge tool names. These are the only MCP-owned tools advertised to the
// model; deferred MCP tools (mcp__<server>__<remote>) are discovered through
// tool_search, described through tool_describe, and invoked through tool_call.
const (
	ToolSearch   = "tool_search"
	ToolDescribe = "tool_describe"
	ToolCall     = "tool_call"
)

const (
	maxSearchQueries   = 7
	defaultSearchLimit = 5
	maxSearchLimit     = 25
	maxDescribeNames   = 10
	maxBridgeCalls     = 10

	maxBridgeResultChars = 16000
	// maxEmbeddedListingRunes bounds the optional deferred-tool listing
	// appended to the tool_search description when the catalog is small.
	maxEmbeddedListingRunes = 1500
)

const toolSearchDescriptionTemplate = "Search {N} additional tools that are loaded on demand. Takes a list of queries searched in parallel against the same catalog; send one query per distinct capability you need. Queries are keyword searches, not questions: the app or service name plus an action and object, no filler words (`gmail send email`, `nvidia driver status`, not `what's my GPU driver version?`); a word no tool contains makes the query return nothing. Returns matching tool names grouped per query plus a shared map with each tool's description. Follow with `tool_describe` to load full parameter schemas, then `tool_call` to invoke. Tools listed at the top of this system prompt are already available and do not need to be searched."

// ProviderView is the request-visible bridge surface: deferred MCP tools
// bridged through tool_search, tool_describe, and tool_call, optionally
// scoped to named servers.
type ProviderView interface {
	BridgeTools(ctx context.Context, principal identity.Principal) []llm.Tool
	Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}) (ExecutionResult, bool, error)
	ToolPolicy(name string) governance.ToolPolicy
	// WithServers returns a view scoped to the named servers. Nil exposes
	// every server.
	WithServers(allow []string) ProviderView
}

// Provider exposes deferred MCP tools through the tool_search, tool_describe,
// and tool_call bridge. No per-server discovery tools or direct deferred
// tools are advertised; the bridge is the only model-visible MCP surface.
type Provider struct {
	manager       *Manager
	reservedTools map[string]bool
	// servers scopes the bridge to named servers. Nil exposes every server;
	// matching is exact against the configured server name.
	servers []string
}

func NewProvider(manager *Manager, reservedToolNames ...string) *Provider {
	provider := &Provider{manager: manager, reservedTools: make(map[string]bool, len(reservedToolNames))}
	for _, name := range reservedToolNames {
		name = strings.TrimSpace(name)
		if name != "" {
			provider.reservedTools[name] = true
		}
	}
	return provider
}

// WithServers returns a view of the provider scoped to the named servers.
// A nil allowlist exposes every server. The view shares the manager; only
// the catalog filter differs, so request-scoped views never disturb the
// shared provider.
func (p *Provider) WithServers(allow []string) ProviderView {
	if p == nil {
		return nil
	}
	q := *p
	if allow == nil {
		q.servers = nil
	} else {
		q.servers = append(make([]string, 0, len(allow)), allow...)
	}
	return &q
}

// BridgeTools returns the three bridge definitions for an authenticated
// principal. The tool_search description carries the live deferred-tool count
// and, when small enough, an embedded tool listing.
func (p *Provider) BridgeTools(ctx context.Context, principal identity.Principal) []llm.Tool {
	if p == nil || p.manager == nil || p.manager.store == nil || !principal.Authenticated() {
		return nil
	}
	catalog, _ := p.deferredCatalog(ctx, principal.CanonicalUserID)
	description := strings.Replace(toolSearchDescriptionTemplate, "{N}", strconv.Itoa(len(catalog)), 1)
	if listing := embeddedToolListing(catalog); listing != "" {
		description += "\nAvailable deferred tools: " + listing
	}
	return []llm.Tool{
		{Type: "function", Function: llm.ToolDefinition{
			Name:        ToolSearch,
			Description: description,
			Parameters: llm.ToolParameters{
				Type: "object",
				Properties: map[string]llm.ToolParameterProperty{
					"queries": {Type: "array", Description: "Keyword queries, one per capability: app or service name + action + object (e.g. ['github create issue', 'slack send message', 'gmail fetch emails']). Not questions or sentences: every word must appear in tool text, or the query returns nothing. Searched in parallel; results come back grouped per query. A single string is accepted and treated as one query.", Items: &llm.ToolParameterProperty{Type: "string"}},
					"limit":   {Type: "integer", Description: "Maximum number of matches per query. Defaults to 5 and is clamped to the configured maximum (25 by default).", Minimum: floatPtr(1), Maximum: floatPtr(25)},
				},
				Required: []string{"queries"},
			},
		}},
		{Type: "function", Function: llm.ToolDefinition{
			Name:        ToolDescribe,
			Description: "Load the full JSON schemas for tools returned by `tool_search`. Required before `tool_call` if a tool's parameters are unknown. Batch every schema you need into one call.",
			Parameters: llm.ToolParameters{
				Type: "object",
				Properties: map[string]llm.ToolParameterProperty{
					"names": {Type: "array", Description: "Exact tool names (as returned by tool_search). A single string is accepted and treated as one name.", Items: &llm.ToolParameterProperty{Type: "string"}},
				},
				Required: []string{"names"},
			},
		}},
		{Type: "function", Function: llm.ToolDefinition{
			Name:        ToolCall,
			Description: "Invoke deferred tools. Takes `calls`, an array of {name, arguments} — one entry per invocation; a single call is an array of one. Independent calls may be batched together. Local entries are split into individual tool calls by the agent and follow each tool's concurrency policy. Results are returned in input order. Argument shapes match each tool's schema (see `tool_describe`). Policy, hooks, and approvals run as for directly-listed tools.",
			Parameters: llm.ToolParameters{
				Type: "object",
				Properties: map[string]llm.ToolParameterProperty{
					"calls": {Type: "array", Description: "One or more independent deferred tool invocations.", Items: &llm.ToolParameterProperty{
						Type: "object",
						Properties: map[string]llm.ToolParameterProperty{
							"name":      {Type: "string", Description: "Exact tool name to invoke."},
							"arguments": {Type: "object", Description: "Arguments matching the tool schema."},
						},
						Required: []string{"name", "arguments"},
					}},
				},
				Required: []string{"calls"},
			},
		}},
	}
}

// Execute runs one bridge tool call. Deferred tools are never executed
// directly; they resolve only through tool_call against the live catalog.
func (p *Provider) Execute(ctx context.Context, principal identity.Principal, name string, args map[string]interface{}) (ExecutionResult, bool, error) {
	if p == nil || p.manager == nil || !principal.Authenticated() {
		return ExecutionResult{}, false, nil
	}
	switch strings.TrimSpace(name) {
	case ToolSearch:
		result, err := p.search(ctx, principal, args)
		return ExecutionResult{Result: result, IsDiscovery: true}, true, err
	case ToolDescribe:
		result, err := p.describe(ctx, principal, args)
		return ExecutionResult{Result: result, IsDiscovery: true}, true, err
	case ToolCall:
		result, err := p.call(ctx, principal, args)
		return ExecutionResult{Result: result}, true, err
	}
	return ExecutionResult{}, false, nil
}

// ToolPolicy returns the request-governance policy for a bridge tool.
func (p *Provider) ToolPolicy(name string) governance.ToolPolicy {
	policy := governance.ToolPolicy{History: governance.HistoryPolicy{Mode: governance.HistoryFull, SearchResult: true}}
	switch strings.TrimSpace(name) {
	case ToolSearch:
		policy.NormalizeArgs = func(args map[string]interface{}) interface{} {
			queries, _ := stringListArg(args, "queries")
			normalized := make([]string, 0, len(queries))
			for _, query := range queries {
				if query = strings.ToLower(strings.Join(strings.Fields(query), " ")); query != "" {
					normalized = append(normalized, query)
				}
			}
			sort.Strings(normalized)
			limit, _ := intArg(args, "limit")
			return map[string]interface{}{"queries": normalized, "limit": limit}
		}
	case ToolDescribe:
		policy.NormalizeArgs = func(args map[string]interface{}) interface{} {
			names, _ := stringListArg(args, "names")
			normalized := make([]string, 0, len(names))
			seen := map[string]bool{}
			for _, name := range names {
				if name = strings.TrimSpace(name); name != "" && !seen[name] {
					seen[name] = true
					normalized = append(normalized, name)
				}
			}
			sort.Strings(normalized)
			return map[string]interface{}{"names": normalized}
		}
	}
	return policy
}

func floatPtr(value float64) *float64 { return &value }

// deferredCatalog rebuilds the stateless deferred-tool catalog from the live
// visible tool specs on every call. Reserved and malformed entries are
// dropped; out-of-scope servers are dropped on scoped views; the result is
// sorted by name.
func (p *Provider) deferredCatalog(ctx context.Context, userID string) ([]registry.CatalogEntry, error) {
	specs := p.manager.ToolSpecs(ctx, userID)
	entries := make([]registry.CatalogEntry, 0, len(specs))
	for _, spec := range specs {
		if isReservedServerName(spec.Server) || isReservedToolName(spec.Name) || p.reservedTools[spec.Name] {
			continue
		}
		if !isBridgeToolName(spec.Name) {
			continue
		}
		entries = append(entries, registry.CatalogEntry{Name: spec.Name, Description: spec.Description, Source: registry.ToolSourceMCP, Server: spec.Server, Parameters: mapParams(spec.Parameters)})
	}
	if p.servers != nil {
		allowed := make(map[string]bool, len(p.servers))
		for _, server := range p.servers {
			allowed[server] = true
		}
		scoped := make([]registry.CatalogEntry, 0, len(entries))
		for _, entry := range entries {
			if allowed[entry.Server] {
				scoped = append(scoped, entry)
			}
		}
		entries = scoped
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func embeddedToolListing(catalog []registry.CatalogEntry) string {
	var builder strings.Builder
	for _, entry := range catalog {
		segment := entry.Name
		if description := strings.TrimSpace(entry.Description); description != "" {
			segment += " (" + description + ")"
		}
		segment += "; "
		if len([]rune(builder.String()+segment)) > maxEmbeddedListingRunes {
			return ""
		}
		builder.WriteString(segment)
	}
	return strings.TrimSuffix(strings.TrimSpace(builder.String()), ";")
}

type bridgeSearchMatch struct {
	Query   string   `json:"query"`
	Matches []string `json:"matches"`
}

type bridgeToolSummary struct {
	Source      string   `json:"source"`
	SourceName  string   `json:"source_name"`
	Description string   `json:"description,omitempty"`
	Required    []string `json:"required,omitempty"`
}

type bridgeSearchData struct {
	Queries          []string                     `json:"queries"`
	TotalAvailable   int                          `json:"total_available"`
	Results          []bridgeSearchMatch          `json:"results"`
	Tools            map[string]bridgeToolSummary `json:"tools"`
	AvailableSources []string                     `json:"available_sources,omitempty"`
	Hint             string                       `json:"hint,omitempty"`
}

func (p *Provider) search(ctx context.Context, principal identity.Principal, args map[string]interface{}) (governance.Result, error) {
	rawQueries, ok := coerceStringList(args["queries"])
	if !ok {
		return governance.Result{}, fmt.Errorf("tool_search requires queries as a string or an array of strings")
	}
	queries := make([]string, 0, len(rawQueries))
	for _, query := range rawQueries {
		if query = strings.Join(strings.Fields(query), " "); query != "" {
			queries = append(queries, query)
		}
		if len(queries) == maxSearchQueries {
			break
		}
	}
	if len(queries) == 0 {
		return governance.Result{}, fmt.Errorf("tool_search requires at least one non-empty query")
	}
	limit := defaultSearchLimit
	if raw, present := args["limit"]; present {
		value, ok := coerceInt(raw)
		if !ok {
			return governance.Result{}, fmt.Errorf("tool_search limit must be an integer")
		}
		limit = min(max(value, 1), maxSearchLimit)
	}
	catalog, err := p.deferredCatalog(ctx, principal.CanonicalUserID)
	if err != nil {
		return governance.Result{}, err
	}
	data := bridgeSearchData{Queries: queries, TotalAvailable: len(catalog), Results: make([]bridgeSearchMatch, 0, len(queries)), Tools: map[string]bridgeToolSummary{}}
	matchedAny := false
	for _, query := range queries {
		matches := searchCatalog(catalog, query, limit)
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Name)
			if _, exists := data.Tools[match.Name]; !exists {
				data.Tools[match.Name] = bridgeToolSummary{Source: match.Source, SourceName: match.Server, Description: match.Description, Required: requiredParams(match.Parameters)}
			}
		}
		if len(names) > 0 {
			matchedAny = true
		}
		data.Results = append(data.Results, bridgeSearchMatch{Query: query, Matches: names})
	}
	if !matchedAny {
		data.AvailableSources = catalogSources(catalog)
		data.Hint = "No tool contains every query word. Retry with the service name plus an action and object (for example 'github create issue'); do not claim the capability is missing."
	}
	if p.manager.log != nil {
		meta := requestctx.MetadataFromContext(ctx)
		p.manager.log.Agent("agent.tool.mcp.search", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...).
			Debug("agent.tool.mcp.search", "searched deferred MCP tools", config.F("query_count", len(queries)), config.F("tool_count", len(data.Tools)), config.F("total_available", len(catalog)))
	}
	return encodeBoundedResult(data, truncateSearchData, matchedAny)
}

func catalogSources(catalog []registry.CatalogEntry) []string {
	seen := map[string]bool{}
	var sources []string
	for _, entry := range catalog {
		if server := strings.TrimSpace(entry.Server); server != "" && !seen[server] {
			seen[server] = true
			sources = append(sources, server)
		}
	}
	sort.Strings(sources)
	return sources
}

func truncateSearchData(data bridgeSearchData) bridgeSearchData {
	for len(data.Results) > 0 {
		longest := -1
		for i, result := range data.Results {
			if len(result.Matches) > 1 && (longest < 0 || len(result.Matches) > len(data.Results[longest].Matches)) {
				longest = i
			}
		}
		if longest < 0 {
			break
		}
		data.Results[longest].Matches = data.Results[longest].Matches[:len(data.Results[longest].Matches)-1]
		used := map[string]bool{}
		for _, result := range data.Results {
			for _, name := range result.Matches {
				used[name] = true
			}
		}
		for name := range data.Tools {
			if !used[name] {
				delete(data.Tools, name)
			}
		}
		if encoded, err := json.Marshal(data); err == nil && len([]rune(string(encoded))) <= maxBridgeResultChars {
			break
		}
	}
	return data
}

// searchCatalog returns up to limit catalog entries matching every query term.
// Terms match case-insensitively against the tool name, server, description,
// and parameter names/descriptions; a term no tool contains yields no hit.
func searchCatalog(catalog []registry.CatalogEntry, query string, limit int) []registry.CatalogEntry {
	terms := strings.Fields(strings.ToLower(query))
	type scoredEntry struct {
		entry registry.CatalogEntry
		score int
	}
	var matches []scoredEntry
	for _, tool := range catalog {
		score, ok := bridgeMatchScore(tool, terms)
		if !ok {
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
	tools := make([]registry.CatalogEntry, 0, min(limit, len(matches)))
	for i := 0; i < len(matches) && len(tools) < limit; i++ {
		tools = append(tools, matches[i].entry)
	}
	return tools
}

func bridgeMatchScore(tool registry.CatalogEntry, terms []string) (int, bool) {
	if len(terms) == 0 {
		return 0, false
	}
	name := strings.ToLower(tool.Name)
	server := strings.ToLower(tool.Server)
	description := strings.ToLower(tool.Description)
	paramText := ""
	for _, param := range tool.Parameters {
		paramText += " " + strings.ToLower(param.Name) + " " + strings.ToLower(param.Description)
	}
	score := 0
	for _, term := range terms {
		termScore := 0
		switch {
		case strings.Contains(name, term):
			termScore = 20
		case strings.Contains(server, term):
			termScore = 12
		case strings.Contains(description, term):
			termScore = 10
		case strings.Contains(paramText, term):
			termScore = 4
		default:
			return 0, false
		}
		score += termScore
	}
	return score, true
}

type bridgeToolSchema struct {
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type bridgeDescribeData struct {
	Tools    map[string]bridgeToolSchema `json:"tools"`
	NotFound []string                    `json:"not_found,omitempty"`
	Errors   map[string]string           `json:"errors,omitempty"`
}

func (p *Provider) describe(ctx context.Context, principal identity.Principal, args map[string]interface{}) (governance.Result, error) {
	rawNames, ok := coerceStringList(args["names"])
	if !ok {
		return governance.Result{}, fmt.Errorf("tool_describe requires names as a string or an array of strings")
	}
	names := make([]string, 0, len(rawNames))
	seen := map[string]bool{}
	for _, name := range rawNames {
		if name = strings.TrimSpace(name); name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		if len(names) == maxDescribeNames {
			break
		}
	}
	if len(names) == 0 {
		return governance.Result{}, fmt.Errorf("tool_describe requires at least one tool name")
	}
	catalog, err := p.deferredCatalog(ctx, principal.CanonicalUserID)
	if err != nil {
		return governance.Result{}, err
	}
	byName := make(map[string]registry.CatalogEntry, len(catalog))
	for _, entry := range catalog {
		byName[entry.Name] = entry
	}
	data := bridgeDescribeData{Tools: map[string]bridgeToolSchema{}}
	for _, name := range names {
		if name == ToolSearch || name == ToolDescribe || name == ToolCall {
			if data.Errors == nil {
				data.Errors = map[string]string{}
			}
			data.Errors[name] = "This is a bridge tool. Call it directly instead of describing it."
			continue
		}
		if p.reservedTools[name] {
			if data.Errors == nil {
				data.Errors = map[string]string{}
			}
			data.Errors[name] = "This tool is already listed. Call it directly instead of using the bridge."
			continue
		}
		entry, ok := byName[name]
		if !ok {
			data.NotFound = append(data.NotFound, name)
			continue
		}
		data.Tools[name] = bridgeToolSchema{Description: entry.Description, Parameters: catalogParameters(entry.Parameters)}
	}
	if len(data.Tools) == 0 && len(data.Errors) == 0 {
		return governance.Result{Content: encodeDescribeResult(data), Outcome: governance.OutcomeUnproductive, ReasonCode: "not_found"}, nil
	}
	if p.manager.log != nil {
		meta := requestctx.MetadataFromContext(ctx)
		p.manager.log.Agent("agent.tool.mcp.describe", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...).
			Debug("agent.tool.mcp.describe", "described deferred MCP tools", config.F("name_count", len(names)), config.F("tool_count", len(data.Tools)), config.F("not_found_count", len(data.NotFound)))
	}
	return governance.Result{Content: encodeDescribeResult(data), Outcome: governance.OutcomeProductive}, nil
}

func catalogParameters(params []registry.ParamSpec) map[string]any {
	properties := make(map[string]any, len(params))
	required := []string{}
	for _, param := range params {
		schema := map[string]any{"type": param.Type}
		if param.Description != "" {
			schema["description"] = param.Description
		}
		if len(param.Enum) > 0 {
			schema["enum"] = param.Enum
		}
		properties[param.Name] = schema
		if param.Required {
			required = append(required, param.Name)
		}
	}
	sort.Strings(required)
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

type bridgeCallEntry struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type bridgeCallEntryResult struct {
	Name    string `json:"name"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

type bridgeCallData struct {
	Results []bridgeCallEntryResult `json:"results"`
}

func (p *Provider) call(ctx context.Context, principal identity.Principal, args map[string]interface{}) (governance.Result, error) {
	entries, err := parseBridgeCalls(args)
	if err != nil {
		return governance.Result{}, err
	}
	catalog, err := p.deferredCatalog(ctx, principal.CanonicalUserID)
	if err != nil {
		return governance.Result{}, err
	}
	byName := make(map[string]registry.CatalogEntry, len(catalog))
	for _, entry := range catalog {
		byName[entry.Name] = entry
	}
	data := bridgeCallData{Results: make([]bridgeCallEntryResult, 0, len(entries))}
	succeeded := 0
	for _, entry := range entries {
		result := bridgeCallEntryResult{Name: entry.Name}
		switch catalogEntry, ok := byName[entry.Name]; {
		case entry.Name == ToolSearch || entry.Name == ToolDescribe || entry.Name == ToolCall:
			result.Error = "Bridge tools cannot invoke themselves. Call the bridge tool directly."
		case !ok:
			result.Error = "Tool is not in scope. Re-run tool_search to find an available deferred tool."
		case validateBridgeArguments(catalogEntry.Parameters, entry.Arguments) != nil:
			result.Error = validateBridgeArguments(catalogEntry.Parameters, entry.Arguments).Error()
		default:
			execution, err := p.manager.Execute(ctx, principal.CanonicalUserID, entry.Name, entry.Arguments)
			if err != nil {
				result.Error = config.SafeErrorText(err)
			} else {
				result.Content = governance.RemoteResultText(entry.Name, execution.Content, maxBridgeResultChars)
				succeeded++
			}
		}
		data.Results = append(data.Results, result)
	}
	outcome := governance.OutcomeProductive
	reason := ""
	if succeeded == 0 {
		outcome = governance.OutcomeUnproductive
		reason = "all_calls_failed"
	}
	if p.manager.log != nil {
		meta := requestctx.MetadataFromContext(ctx)
		p.manager.log.Agent("agent.tool.mcp.call", meta.RequestID, principal.CanonicalUserID, principal.Gateway, meta.Model).With(requestctx.LogFields(ctx)...).
			Debug("agent.tool.mcp.call", "invoked deferred MCP tools", config.F("call_count", len(entries)), config.F("success_count", succeeded))
	}
	if len(data.Results) == 1 && data.Results[0].Error == "" {
		return governance.Result{Content: data.Results[0].Content, ResultName: data.Results[0].Name, Outcome: outcome, ReasonCode: reason}, nil
	}
	return encodeBoundedCallResult(data, outcome, reason)
}

func encodeBoundedCallResult(data bridgeCallData, outcome governance.Outcome, reason string) (governance.Result, error) {
	if encoded, err := json.Marshal(data); err == nil && len([]rune(string(encoded))) <= maxBridgeResultChars {
		return governance.Result{Content: string(encoded), Outcome: outcome, ReasonCode: reason}, nil
	}
	for {
		largest, size := -1, 0
		for i := range data.Results {
			if runes := []rune(data.Results[i].Content); len(runes) > size {
				largest, size = i, len(runes)
			}
		}
		if largest < 0 {
			break
		}
		data.Results[largest].Content = governance.BoundResultText(data.Results[largest].Content, size/2)
		if encoded := encodeBridgeData(data, true); len([]rune(encoded)) <= maxBridgeResultChars {
			return governance.Result{Content: encoded, Outcome: outcome, ReasonCode: reason}, nil
		}
	}
	return governance.Result{}, fmt.Errorf("bridge result metadata exceeds limit")
}

func parseBridgeCalls(args map[string]interface{}) ([]bridgeCallEntry, error) {
	raw, ok := args["calls"]
	if !ok {
		return nil, fmt.Errorf("tool_call requires calls as an array of {name, arguments}")
	}
	items, ok := raw.([]interface{})
	if single, isSingle := raw.(map[string]interface{}); !ok && isSingle {
		items = []interface{}{single}
		ok = true
	}
	if !ok {
		return nil, fmt.Errorf("tool_call requires calls as an array of {name, arguments}")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("tool_call requires at least one call")
	}
	if len(items) > maxBridgeCalls {
		return nil, fmt.Errorf("tool_call accepts at most %d calls per invocation", maxBridgeCalls)
	}
	entries := make([]bridgeCallEntry, 0, len(items))
	for i, item := range items {
		call, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("tool_call entry %d must be an object with name and arguments", i+1)
		}
		name, _ := call["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("tool_call entry %d requires a tool name", i+1)
		}
		arguments, _ := call["arguments"].(map[string]interface{})
		if arguments == nil {
			if _, present := call["arguments"]; present {
				return nil, fmt.Errorf("tool_call entry %d arguments must be an object", i+1)
			}
			arguments = map[string]interface{}{}
		}
		entries = append(entries, bridgeCallEntry{Name: strings.TrimSpace(name), Arguments: arguments})
	}
	return entries, nil
}

func validateBridgeArguments(params []registry.ParamSpec, args map[string]interface{}) error {
	for _, param := range params {
		if !param.Required {
			continue
		}
		value, ok := args[param.Name]
		if !ok || value == nil {
			return fmt.Errorf("missing required argument %q", param.Name)
		}
		if text, isString := value.(string); isString && strings.TrimSpace(text) == "" {
			return fmt.Errorf("missing required argument %q", param.Name)
		}
	}
	return nil
}

func encodeBridgeData(data any, truncated bool) string {
	encoded, err := json.Marshal(data)
	if err != nil {
		return `{"error":"unavailable"}`
	}
	if truncated {
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &fields) != nil || fields == nil {
			return `{"error":"unavailable"}`
		}
		fields["truncated"] = json.RawMessage("true")
		encoded, _ = json.Marshal(fields)
	}
	return string(encoded)
}

func encodeBoundedResult[T any](data T, truncate func(T) T, matched bool) (governance.Result, error) {
	if encoded, err := json.Marshal(data); err == nil && len([]rune(string(encoded))) <= maxBridgeResultChars {
		if matched {
			return governance.Result{Content: string(encoded), Outcome: governance.OutcomeProductive}, nil
		}
		return governance.Result{Content: string(encoded), Outcome: governance.OutcomeUnproductive, ReasonCode: "no_results"}, nil
	}
	data = truncate(data)
	encoded := encodeBridgeData(data, true)
	if len([]rune(encoded)) > maxBridgeResultChars {
		return governance.Result{}, fmt.Errorf("bridge search result exceeds limit")
	}
	if matched {
		return governance.Result{Content: encoded, Outcome: governance.OutcomeProductive}, nil
	}
	return governance.Result{Content: encoded, Outcome: governance.OutcomeUnproductive, ReasonCode: "no_results"}, nil
}

func encodeDescribeResult(data bridgeDescribeData) string {
	truncated := false
	for {
		encoded := encodeBridgeData(data, truncated)
		if len([]rune(encoded)) <= maxBridgeResultChars {
			return encoded
		}
		// Never advertise a partial schema. Omit the largest complete schema
		// with an explicit error, preserving navigation to all other entries.
		largest, size := "", 0
		for name, schema := range data.Tools {
			encoded, _ := json.Marshal(schema)
			if len(encoded) > size || (len(encoded) == size && name < largest) {
				largest, size = name, len(encoded)
			}
		}
		if largest == "" {
			return `{"error":"schema metadata exceeds limit","truncated":true}`
		}
		delete(data.Tools, largest)
		if data.Errors == nil {
			data.Errors = map[string]string{}
		}
		data.Errors[largest] = "Schema omitted because the result limit was reached. Describe this tool individually."
		truncated = true
	}
}

func mustMarshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":"unavailable"}`)
	}
	return encoded
}

func filterReservedCatalog(entries []registry.CatalogEntry, reserved map[string]bool) []registry.CatalogEntry {
	if len(reserved) == 0 {
		return entries
	}
	filtered := make([]registry.CatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if !reserved[entry.Name] {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func mapParams(params []ParamSpec) []registry.ParamSpec {
	out := make([]registry.ParamSpec, 0, len(params))
	for _, p := range params {
		out = append(out, registry.ParamSpec{Name: p.Name, Type: p.Type, Required: p.Required, Description: p.Description, Enum: p.Enum})
	}
	return out
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

func coerceStringList(value any) ([]string, bool) {
	switch typed := value.(type) {
	case string:
		return []string{typed}, true
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	case []string:
		return append([]string(nil), typed...), true
	}
	return nil, false
}

func stringListArg(args map[string]interface{}, name string) ([]string, bool) {
	value, ok := args[name]
	if !ok {
		return nil, false
	}
	return coerceStringList(value)
}

func coerceInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

func intArg(args map[string]interface{}, name string) (int, bool) {
	value, ok := args[name]
	if !ok {
		return 0, false
	}
	return coerceInt(value)
}

func stringArg(args map[string]interface{}, name string) string {
	value, _ := args[name].(string)
	return value
}
