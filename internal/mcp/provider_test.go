package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func testPrincipal(userID string) identity.Principal {
	return identity.Principal{CanonicalUserID: userID, Gateway: "imessage", ExternalID: userID, Assurance: identity.AssuranceBlueBubblesWebhook}
}

func fixtureHandler(content string) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: content, Outcome: governance.OutcomeProductive}, nil
	}
}

func bridgeTestManager(t *testing.T, owner string) *Manager {
	t.Helper()
	store := testStore(t)
	addTestUsers(t, store, owner)
	ctx := context.Background()
	cfg, err := store.Save(ctx, ServerConfig{Scope: ScopeUser, OwnerUserID: owner, Name: "home", Transport: TransportStreamableHTTP, URL: "https://example.com/home", Enabled: true})
	if err != nil {
		t.Fatalf("save home: %v", err)
	}
	manager := newManager(store, config.NewLogger(config.LevelError))
	manager.sessions[scopeKey(cfg)] = &server{config: cfg, tools: []ToolSpec{
		{Name: "mcp__home__turn_on", Server: "home", RemoteName: "turn_on", Description: "Turn on a light", Parameters: []ParamSpec{{Name: "entity", Type: "string", Required: true}}, Handler: fixtureHandler("light turned on")},
		{Name: "mcp__home__weather", Server: "home", RemoteName: "weather", Description: "Read weather", Handler: fixtureHandler("sunny")},
	}}
	return manager
}

func TestBridgeToolsAdvertiseThreeTools(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	tools := provider.BridgeTools(context.Background(), testPrincipal("user_1"))
	if len(tools) != 3 {
		t.Fatalf("bridge tools = %d, want 3", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Function.Name] = true
	}
	if !names[ToolSearch] || !names[ToolDescribe] || !names[ToolCall] {
		t.Fatalf("bridge tools = %+v", names)
	}
	for _, tool := range tools {
		if tool.Function.Name == ToolSearch {
			if _, ok := tool.Function.Parameters.Properties["queries"]; !ok {
				t.Fatal("tool_search schema missing queries parameter")
			}
			if _, ok := tool.Function.Parameters.Properties["limit"]; !ok {
				t.Fatal("tool_search schema missing limit parameter")
			}
			if len(tool.Function.Parameters.Required) != 1 || tool.Function.Parameters.Required[0] != "queries" {
				t.Fatalf("tool_search required = %+v", tool.Function.Parameters.Required)
			}
			if !strings.Contains(tool.Function.Description, "2 additional tools") {
				t.Fatalf("tool_search description missing live count: %q", tool.Function.Description)
			}
		}
		if tool.Function.Name == ToolDescribe {
			if _, ok := tool.Function.Parameters.Properties["names"]; !ok {
				t.Fatal("tool_describe schema missing names parameter")
			}
		}
		if tool.Function.Name == ToolCall {
			if _, ok := tool.Function.Parameters.Properties["calls"]; !ok {
				t.Fatal("tool_call schema missing calls parameter")
			}
		}
	}
}

func TestBridgeToolsRejectUnauthenticatedPrincipal(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	principal := testPrincipal("user_1")
	principal.Assurance = identity.AssuranceSelfAsserted
	if tools := provider.BridgeTools(context.Background(), principal); len(tools) != 0 {
		t.Fatalf("unauthenticated bridge tools: %+v", tools)
	}
	if _, handled, _ := provider.Execute(context.Background(), principal, ToolSearch, map[string]interface{}{"queries": "light"}); handled {
		t.Fatal("unauthenticated search was handled")
	}
}

func TestBridgeSearchFindsByKeywordAcrossServers(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	result, handled, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolSearch, map[string]interface{}{"queries": []interface{}{"light turn"}})
	if err != nil || !handled {
		t.Fatalf("search handled=%t err=%v", handled, err)
	}
	if result.Outcome != governance.OutcomeProductive {
		t.Fatalf("search outcome = %q: %s", result.Outcome, result.Content)
	}
	var envelope struct {
		Data bridgeSearchData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatalf("search result is not JSON: %v", err)
	}
	if envelope.Data.TotalAvailable != 2 {
		t.Fatalf("total_available = %d, want 2", envelope.Data.TotalAvailable)
	}
	if len(envelope.Data.Results) != 1 || len(envelope.Data.Results[0].Matches) != 1 || envelope.Data.Results[0].Matches[0] != "mcp__home__turn_on" {
		t.Fatalf("search results = %+v", envelope.Data.Results)
	}
	summary, ok := envelope.Data.Tools["mcp__home__turn_on"]
	if !ok || summary.Source != "mcp" || summary.SourceName != "home" || len(summary.Required) != 1 || summary.Required[0] != "entity" {
		t.Fatalf("search tool summary = %+v", envelope.Data.Tools)
	}
}

func TestBridgeSearchUnknownWordReturnsHintNotMissing(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	result, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolSearch, map[string]interface{}{"queries": "zxqyblorp"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != governance.OutcomeUnproductive || result.ReasonCode != "no_results" {
		t.Fatalf("empty search outcome = %q reason = %q", result.Outcome, result.ReasonCode)
	}
	var envelope struct {
		Data bridgeSearchData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.AvailableSources) != 1 || envelope.Data.AvailableSources[0] != "home" {
		t.Fatalf("available sources = %+v", envelope.Data.AvailableSources)
	}
	if !strings.Contains(envelope.Data.Hint, "Retry") {
		t.Fatalf("empty search hint = %q", envelope.Data.Hint)
	}
}

func TestBridgeSearchEnforcesQueryCapAndLimit(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	many := make([]interface{}, 0, 10)
	for i := 0; i < 10; i++ {
		many = append(many, "light")
	}
	result, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolSearch, map[string]interface{}{"queries": many, "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data bridgeSearchData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Queries) != maxSearchQueries {
		t.Fatalf("query count = %d, want %d", len(envelope.Data.Queries), maxSearchQueries)
	}
	if _, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolSearch, map[string]interface{}{}); err == nil {
		t.Fatal("missing queries accepted")
	}
}

func TestBridgeSearchIsScopedToPrincipal(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	result, _, err := provider.Execute(context.Background(), testPrincipal("user_2"), ToolSearch, map[string]interface{}{"queries": "light"})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data bridgeSearchData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.TotalAvailable != 0 || len(envelope.Data.Tools) != 0 {
		t.Fatalf("another user's catalog leaked: %+v", envelope.Data)
	}
}

func TestBridgeDescribeReturnsSchemasNotFoundAndErrors(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"), "memory")
	result, handled, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolDescribe, map[string]interface{}{"names": []interface{}{"mcp__home__turn_on", "mcp__home__missing", "memory", "tool_search"}})
	if err != nil || !handled {
		t.Fatalf("describe handled=%t err=%v", handled, err)
	}
	var envelope struct {
		Data bridgeDescribeData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	schema, ok := envelope.Data.Tools["mcp__home__turn_on"]
	if !ok {
		t.Fatalf("describe tools = %+v", envelope.Data.Tools)
	}
	props, ok := schema.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("describe parameters = %+v", schema.Parameters)
	}
	if _, ok := props["entity"]; !ok {
		t.Fatalf("describe properties = %+v", props)
	}
	if len(envelope.Data.NotFound) != 1 || envelope.Data.NotFound[0] != "mcp__home__missing" {
		t.Fatalf("not_found = %+v", envelope.Data.NotFound)
	}
	if len(envelope.Data.Errors) != 2 {
		t.Fatalf("errors = %+v", envelope.Data.Errors)
	}
}

func TestBridgeDescribeAllUnknownIsUnproductive(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	result, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolDescribe, map[string]interface{}{"names": "mcp__home__missing"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != governance.OutcomeUnproductive {
		t.Fatalf("describe outcome = %q", result.Outcome)
	}
}

func TestBridgeCallInvokesInOrderAndReportsEntryErrors(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	result, handled, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolCall, map[string]interface{}{"calls": []interface{}{
		map[string]interface{}{"name": "mcp__home__weather", "arguments": map[string]interface{}{}},
		map[string]interface{}{"name": "mcp__home__missing", "arguments": map[string]interface{}{}},
		map[string]interface{}{"name": "mcp__home__turn_on", "arguments": map[string]interface{}{}},
	}})
	if err != nil || !handled {
		t.Fatalf("call handled=%t err=%v", handled, err)
	}
	var envelope struct {
		Data bridgeCallData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatalf("call result is not JSON: %v", err)
	}
	if len(envelope.Data.Results) != 3 {
		t.Fatalf("call results = %+v", envelope.Data.Results)
	}
	for i, name := range []string{"mcp__home__weather", "mcp__home__missing", "mcp__home__turn_on"} {
		if envelope.Data.Results[i].Name != name {
			t.Fatalf("call order = %+v", envelope.Data.Results)
		}
	}
	if !strings.Contains(envelope.Data.Results[0].Content, "sunny") {
		t.Fatalf("weather result = %+v", envelope.Data.Results[0])
	}
	if envelope.Data.Results[1].Error == "" || !strings.Contains(envelope.Data.Results[1].Error, "tool_search") {
		t.Fatalf("out-of-scope error = %+v", envelope.Data.Results[1])
	}
	if envelope.Data.Results[2].Error == "" || !strings.Contains(envelope.Data.Results[2].Error, "entity") {
		t.Fatalf("required-args error = %+v", envelope.Data.Results[2])
	}
	if result.Outcome != governance.OutcomeProductive {
		t.Fatalf("call outcome = %q reason = %q", result.Outcome, result.ReasonCode)
	}
}

func TestBridgeCallAllFailuresIsUnproductive(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	result, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolCall, map[string]interface{}{"calls": []interface{}{
		map[string]interface{}{"name": "mcp__home__missing", "arguments": map[string]interface{}{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != governance.OutcomeUnproductive || result.ReasonCode != "all_calls_failed" {
		t.Fatalf("call outcome = %q reason = %q", result.Outcome, result.ReasonCode)
	}
}

func TestBridgeCallRejectsBridgeSelfInvocationAndEmpty(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	result, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolCall, map[string]interface{}{"calls": []interface{}{
		map[string]interface{}{"name": "tool_search", "arguments": map[string]interface{}{"queries": "x"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data bridgeCallData `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Results) != 1 || envelope.Data.Results[0].Error == "" {
		t.Fatalf("self-invocation results = %+v", envelope.Data.Results)
	}
	if _, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolCall, map[string]interface{}{"calls": []interface{}{}}); err == nil {
		t.Fatal("empty calls accepted")
	}
	if _, _, err := provider.Execute(context.Background(), testPrincipal("user_1"), ToolCall, map[string]interface{}{}); err == nil {
		t.Fatal("missing calls accepted")
	}
}

func TestBridgeDoesNotExecuteDeferredToolsDirectly(t *testing.T) {
	provider := NewProvider(bridgeTestManager(t, "user_1"))
	if _, handled, _ := provider.Execute(context.Background(), testPrincipal("user_1"), "mcp__home__weather", map[string]interface{}{}); handled {
		t.Fatal("deferred tool executed outside tool_call")
	}
	if _, handled, _ := provider.Execute(context.Background(), testPrincipal("user_1"), "home.turn_on", map[string]interface{}{}); handled {
		t.Fatal("legacy dot-qualified tool was handled")
	}
}

func TestBridgeDoesNotAdvertiseReservedSoulServer(t *testing.T) {
	store := testStore(t)
	addTestUsers(t, store, "user_1")
	ctx := context.Background()
	cfg, err := store.Save(ctx, ServerConfig{Scope: ScopeGlobal, Name: "soul", Transport: TransportStreamableHTTP, URL: "https://example.com/mcp", Enabled: true})
	if err != nil {
		t.Fatalf("save soul: %v", err)
	}
	manager := newManager(store, config.NewLogger(config.LevelError))
	manager.sessions[scopeKey(cfg)] = &server{config: cfg, tools: []ToolSpec{{Name: "mcp__soul__read", Server: "soul", RemoteName: "read"}}}
	provider := NewProvider(manager)
	result, _, err := provider.Execute(ctx, testPrincipal("user_1"), ToolSearch, map[string]interface{}{"queries": "read"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, "mcp__soul__read") {
		t.Fatalf("reserved soul tool was advertised: %s", result.Content)
	}
}

func TestBridgeToolPoliciesHaveNoPerToolLimits(t *testing.T) {
	provider := &Provider{}
	for _, name := range []string{ToolSearch, ToolDescribe, ToolCall} {
		policy := provider.ToolPolicy(name)
		if policy.MaxExecutions != 0 || policy.MaxFailures != 0 || policy.MaxUnproductive != 0 {
			t.Fatalf("%s policy has per-tool limits: %+v", name, policy)
		}
		if !policy.BlockDuplicates {
			t.Fatalf("%s does not block completed duplicates", name)
		}
		if policy.History.Mode != governance.HistoryFull || !policy.History.SearchResult {
			t.Fatalf("%s history policy = %+v", name, policy.History)
		}
	}
}

func TestBridgeNamesParseAndRejectLegacy(t *testing.T) {
	if _, _, ok := parseBridgeToolName("mcp__home__turn_on"); !ok {
		t.Fatal("bridge name did not parse")
	}
	if isBridgeToolName("home.turn_on") {
		t.Fatal("legacy name parsed as bridge")
	}
	if server, remote, ok := splitToolName("mcp__home__turn_on"); !ok || server != "home" || remote != "turn_on" {
		t.Fatalf("bridge split = %q %q %t", server, remote, ok)
	}
	if !isReservedToolName("mcp__soul__read") || !isReservedToolName("soul.read") {
		t.Fatal("reserved tool names not detected")
	}
}

func TestProviderFiltersNamesReservedByBuiltinCatalog(t *testing.T) {
	provider := NewProvider(nil, "web.search")
	entries := []registry.CatalogEntry{{Name: "web.search"}, {Name: "web.lookup"}}
	filtered := filterReservedCatalog(entries, provider.reservedTools)
	if len(filtered) != 1 || filtered[0].Name != "web.lookup" {
		t.Fatalf("filtered catalog = %+v", filtered)
	}
	if _, handled, _ := provider.Execute(context.Background(), testPrincipal("user_1"), "web.search", nil); handled {
		t.Fatal("reserved execution was handled")
	}
}
