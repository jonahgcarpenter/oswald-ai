package agent

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

func statelessTestAgent(t *testing.T) *Agent {
	t.Helper()
	log := config.NewLogger(config.LevelError)
	reg := registry.New(log)
	if err := reg.RegisterDefinition(llm.ToolDefinition{Name: "memory", Description: "Test memory tool", Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{"q": {Type: "string"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterHandler("memory", governance.ToolPolicy{}, func(_ context.Context, _ map[string]interface{}) (governance.Result, error) {
		return governance.Result{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return &Agent{registry: reg}
}

func clientTool(name string) llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolDefinition{Name: name, Parameters: llm.ToolParameters{Type: "object"}}}
}

func baseCatalog() offeredToolCatalog {
	return offeredToolCatalog{
		Tools:    []llm.Tool{{Type: "function", Function: llm.ToolDefinition{Name: "memory"}}},
		Policies: map[string]governance.ToolPolicy{"memory": {}},
	}
}

func statelessToolNames(tools []llm.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func TestStatelessBuiltinsDisabledByDefault(t *testing.T) {
	a := statelessTestAgent(t)
	base := baseCatalog()

	catalog, err := a.toolsForStatelessRequest(base, nil, ClientToolChoice{})
	if err != nil || len(catalog.Tools) != 0 {
		t.Fatalf("auto without client tools must offer nothing: tools=%v err=%v", statelessToolNames(catalog.Tools), err)
	}

	catalog, err = a.toolsForStatelessRequest(base, []llm.Tool{clientTool("get_weather")}, ClientToolChoice{Mode: "auto"})
	if err != nil || len(catalog.Tools) != 1 || catalog.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("auto must offer only caller tools: tools=%v err=%v", statelessToolNames(catalog.Tools), err)
	}

	if _, err := a.toolsForStatelessRequest(base, nil, ClientToolChoice{Mode: "required"}); err == nil {
		t.Fatal("required without caller tools must fail")
	}
	catalog, err = a.toolsForStatelessRequest(base, []llm.Tool{clientTool("get_weather")}, ClientToolChoice{Mode: "required"})
	if err != nil || len(catalog.Tools) != 1 || catalog.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("required must offer only caller tools: tools=%v err=%v", statelessToolNames(catalog.Tools), err)
	}

	catalog, err = a.toolsForStatelessRequest(base, []llm.Tool{clientTool("get_weather")}, ClientToolChoice{Mode: "none"})
	if err != nil || len(catalog.Tools) != 0 {
		t.Fatalf("none must offer nothing: tools=%v err=%v", statelessToolNames(catalog.Tools), err)
	}

	catalog, err = a.toolsForStatelessRequest(base, nil, ClientToolChoice{Mode: "named", Named: "memory"})
	if err != nil || len(catalog.Tools) != 1 || catalog.Tools[0].Function.Name != "memory" {
		t.Fatalf("named builtin must opt in: tools=%v err=%v", statelessToolNames(catalog.Tools), err)
	}
	if _, err := a.toolsForStatelessRequest(base, nil, ClientToolChoice{Mode: "named", Named: "unknown"}); err == nil {
		t.Fatal("named unknown must fail")
	}

	catalog, err = a.toolsForStatelessRequest(base, []llm.Tool{clientTool("get_weather")}, ClientToolChoice{Mode: "allowed", Allowed: []string{"get_weather"}, AllowedMode: "auto"})
	if err != nil || len(catalog.Tools) != 1 || catalog.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("allowed client-only must exclude builtins: tools=%v err=%v", statelessToolNames(catalog.Tools), err)
	}
	catalog, err = a.toolsForStatelessRequest(base, nil, ClientToolChoice{Mode: "allowed", Allowed: []string{"memory"}, AllowedMode: "auto"})
	if err != nil || len(catalog.Tools) != 1 || catalog.Tools[0].Function.Name != "memory" {
		t.Fatalf("allowed builtin must opt in: tools=%v err=%v", statelessToolNames(catalog.Tools), err)
	}
	if _, err := a.toolsForStatelessRequest(base, nil, ClientToolChoice{Mode: "allowed", Allowed: []string{"unknown"}, AllowedMode: "auto"}); err == nil {
		t.Fatal("allowed unknown must fail")
	}
}
