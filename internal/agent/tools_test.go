package agent

import (
	"context"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
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

func statelessPrincipal() identity.Principal {
	return identity.Principal{CanonicalUserID: "user-1", Gateway: "openai", ExternalID: identity.LocalOpenAIIdentifier, Assurance: identity.AssuranceLocalLoopback}
}

func TestStatelessForcingReachesProvider(t *testing.T) {
	newTextAgent := func(t *testing.T, chat *fakeChatter) *Agent {
		t.Helper()
		a, _ := newTestAgent(t, chat, nil, nil)
		return a
	}
	t.Run("named forces required and disables parallel", func(t *testing.T) {
		chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
		a := newTextAgent(t, chat)
		_, err := a.Process(context.Background(), Request{Principal: statelessPrincipal(), Prompt: "hi", Stateless: true,
			ClientTools:      []llm.Tool{clientTool("get_weather")},
			ClientToolChoice: ClientToolChoice{Mode: "named", Named: "get_weather"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(chat.requests) != 1 {
			t.Fatalf("model calls=%d, want 1", len(chat.requests))
		}
		req := chat.requests[0]
		if req.ToolChoice != llm.ToolChoiceRequired {
			t.Fatalf("tool choice %q, want required", req.ToolChoice)
		}
		if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
			t.Fatalf("parallel %+v, want disabled", req.ParallelToolCalls)
		}
		if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
			t.Fatal("named must advertise only the forced tool")
		}
	})
	t.Run("explicit parallel false honored", func(t *testing.T) {
		chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
		a := newTextAgent(t, chat)
		disabled := false
		_, err := a.Process(context.Background(), Request{Principal: statelessPrincipal(), Prompt: "hi", Stateless: true,
			ClientTools:             []llm.Tool{clientTool("a"), clientTool("b")},
			ClientToolChoice:        ClientToolChoice{Mode: "auto"},
			ClientParallelToolCalls: &disabled})
		if err != nil {
			t.Fatal(err)
		}
		req := chat.requests[0]
		if req.ToolChoice != "" {
			t.Fatalf("tool choice %q, want auto", req.ToolChoice)
		}
		if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
			t.Fatalf("parallel %+v, want disabled", req.ParallelToolCalls)
		}
	})
	t.Run("required forces first call only", func(t *testing.T) {
		chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
		a := newTextAgent(t, chat)
		_, err := a.Process(context.Background(), Request{Principal: statelessPrincipal(), Prompt: "hi", Stateless: true,
			ClientTools:      []llm.Tool{clientTool("get_weather")},
			ClientToolChoice: ClientToolChoice{Mode: "required"}})
		if err != nil {
			t.Fatal(err)
		}
		if chat.requests[0].ToolChoice != llm.ToolChoiceRequired {
			t.Fatalf("tool choice %q, want required", chat.requests[0].ToolChoice)
		}
	})
}

func TestStatelessGenerationReachesProvider(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	a, _ := newTestAgent(t, chat, nil, nil)
	temperature, topP, frequency, presence := 0.7, 0.9, 0.5, -0.5
	var seed int64 = 7
	cap := 512
	_, err := a.Process(context.Background(), Request{Principal: statelessPrincipal(), Prompt: "hi", Stateless: true,
		ClientGeneration: &ClientGeneration{Temperature: &temperature, TopP: &topP, FrequencyPenalty: &frequency, PresencePenalty: &presence, Seed: &seed, Stop: []string{"END"}, MaxTokens: &cap}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.requests) != 1 {
		t.Fatalf("model calls=%d, want 1", len(chat.requests))
	}
	req := chat.requests[0]
	if req.Temperature == nil || *req.Temperature != temperature || req.TopP == nil || *req.TopP != topP ||
		req.FrequencyPenalty == nil || *req.FrequencyPenalty != frequency || req.PresencePenalty == nil || *req.PresencePenalty != presence ||
		req.Seed == nil || *req.Seed != seed || len(req.Stop) != 1 || req.Stop[0] != "END" ||
		req.MaxTokens == nil || *req.MaxTokens != cap {
		t.Fatalf("generation not applied: %+v", req)
	}
}

func TestStatelessLengthFinishPropagates(t *testing.T) {
	chat := &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", DoneReason: "length", Message: llm.ChatMessage{Role: "assistant", Content: "truncated"}}}}
	a, _ := newTestAgent(t, chat, nil, nil)
	resp, err := a.Process(context.Background(), Request{Principal: statelessPrincipal(), Prompt: "hi", Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "length" {
		t.Fatalf("finish reason %q, want length", resp.FinishReason)
	}
	chat = &fakeChatter{responses: []*llm.ChatResponse{{Model: "test-model", DoneReason: "stop", Message: llm.ChatMessage{Role: "assistant", Content: "done"}}}}
	a, _ = newTestAgent(t, chat, nil, nil)
	resp, err = a.Process(context.Background(), Request{Principal: statelessPrincipal(), Prompt: "hi", Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "" {
		t.Fatalf("finish reason %q, want empty (stop default)", resp.FinishReason)
	}
}
