package registry

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

func definition(name, description string) llm.ToolDefinition {
	return llm.ToolDefinition{Name: name, Description: description, Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{}}}
}

func TestRegistryRegistersDefinitionAndExecutesHandler(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	def := definition("test.echo", "Echo a value.")
	def.Parameters.Properties["text"] = llm.ToolParameterProperty{Type: "string", Description: "Text to echo"}
	def.Parameters.Required = []string{"text"}
	if err := reg.RegisterDefinition(def); err != nil {
		t.Fatal(err)
	}
	if reg.Count() != 1 || !reflect.DeepEqual(reg.Names(), []string{"test.echo"}) {
		t.Fatalf("unexpected registry names: %v", reg.Names())
	}
	if err := reg.RegisterHandler("missing", testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{}, nil
	}); err == nil {
		t.Fatal("expected unknown handler registration error")
	}
	if err := reg.RegisterHandler("test.echo", testToolPolicy(), func(_ context.Context, args map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: args["text"].(string), Outcome: governance.OutcomeProductive}, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Execute(context.Background(), "test.echo", map[string]interface{}{"text": "hello"})
	if err != nil || got.Content != "hello" || got.Outcome != governance.OutcomeProductive {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	tools := reg.LLMTools()
	if len(tools) != 1 || tools[0].Type != "function" || !reflect.DeepEqual(tools[0].Function, def) {
		t.Fatalf("unexpected LLM tools: %+v", tools)
	}
	if policy, ok := reg.Policy("test.echo"); !ok || !reflect.DeepEqual(policy, testToolPolicy()) || !reg.HasHandler("test.echo") {
		t.Fatal("handler policy or registration lost")
	}
}

func TestRegisterDefinitionValidationAndDuplicates(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	for _, def := range []llm.ToolDefinition{
		definition(" ", "description"),
		definition("test.name", " "),
		{Name: "test.name", Description: "description", Parameters: llm.ToolParameters{Type: "array", Properties: map[string]llm.ToolParameterProperty{}}},
		{Name: "test.name", Description: "description", Parameters: llm.ToolParameters{Type: "object"}},
	} {
		if err := reg.RegisterDefinition(def); err == nil || reg.Count() != 0 {
			t.Fatalf("invalid definition accepted: %+v", def)
		}
	}
	def := definition("test.name", "description")
	if err := reg.RegisterDefinition(def); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterDefinition(def); err == nil || reg.Count() != 1 {
		t.Fatal("duplicate definition replaced original")
	}
}

func TestRegistryPreservesNestedSchema(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	def := definition("test.schema", "Schema")
	no := false
	def.Parameters.AdditionalProperties = &no
	def.Parameters.Required = []string{"items"}
	def.Parameters.Properties["items"] = llm.ToolParameterProperty{Type: "array", Items: &llm.ToolParameterProperty{Type: "string", Enum: []string{"a", "b"}}}
	if err := reg.RegisterDefinition(def); err != nil {
		t.Fatal(err)
	}
	if got := reg.LLMTools()[0].Function; !reflect.DeepEqual(got, def) {
		t.Fatalf("schema constraints lost: %+v", got)
	}
}

func TestRegistryUnknownToolListsMatchingPrefixHandlers(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	for _, name := range []string{"files.read", "files.search", "files.list", "files.delete", "web.search"} {
		registerTestTool(t, reg, name)
	}
	_, err := reg.Execute(context.Background(), "files.missing", nil)
	want := `no handler registered for tool "files.missing"; available tools in prefix "files": files.delete, files.list, files.read, files.search`
	if err == nil || err.Error() != want {
		t.Fatalf("error=%v want=%q", err, want)
	}
}

func TestRegistryUnknownToolWithoutPrefixListsAllHandlers(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	for _, name := range []string{"files.read", "files.delete", "web.search"} {
		registerTestTool(t, reg, name)
	}
	_, err := reg.Execute(context.Background(), "delete", nil)
	want := `no handler registered for tool "delete"; available tools: files.delete, files.read, web.search`
	if err == nil || err.Error() != want {
		t.Fatalf("error=%v want=%q", err, want)
	}
}

func TestRegistryUnknownToolWithEmptyPrefixMatchListsNone(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	registerTestTool(t, reg, "files.read")
	registerTestTool(t, reg, "web.search")
	_, err := reg.Execute(context.Background(), "missing.write", nil)
	want := `no handler registered for tool "missing.write"; available tools in prefix "missing": none`
	if err == nil || err.Error() != want {
		t.Fatalf("error=%v want=%q", err, want)
	}
}

func registerTestTool(t *testing.T, reg *Registry, name string) {
	t.Helper()
	if err := reg.RegisterDefinition(definition(name, strings.TrimPrefix(name, "test."))); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterHandler(name, testToolPolicy(), func(context.Context, map[string]interface{}) (governance.Result, error) {
		return governance.Result{Content: "ok", Outcome: governance.OutcomeProductive}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testToolPolicy() governance.ToolPolicy {
	return governance.ToolPolicy{MaxExecutions: 1, MaxFailures: 1, MaxUnproductive: 1}
}

func TestRegistryVisibilityAndOrdering(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	for _, def := range []llm.ToolDefinition{definition("test.second", " Second "), definition("test.first", " First ")} {
		if err := reg.RegisterDefinition(def); err != nil {
			t.Fatal(err)
		}
	}
	tools := reg.LLMTools()
	if len(tools) != 2 || tools[0].Function.Name != "test.first" || tools[1].Function.Name != "test.second" || tools[0].Function.Description != "First" || tools[1].Function.Description != "Second" {
		t.Fatalf("unexpected visible catalog: %+v", tools)
	}
	tools = reg.LLMToolsForVisibility(ToolVisibility{HiddenBuiltins: map[string]bool{"test.first": true}})
	if len(tools) != 1 || tools[0].Function.Name != "test.second" {
		t.Fatalf("request-hidden builtin remained visible: %+v", tools)
	}
}

func TestDisableBuiltinHidesToolButKeepsNameReserved(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	if err := reg.RegisterDefinition(definition("web.search", "Search")); err != nil {
		t.Fatal(err)
	}
	if err := reg.DisableBuiltin("web.search"); err != nil {
		t.Fatal(err)
	}
	if len(reg.LLMTools()) != 0 || !reflect.DeepEqual(reg.Names(), []string{"web.search"}) || len(reg.EnabledBuiltinNames()) != 0 {
		t.Fatal("disabled builtin must be hidden but reserved")
	}
}

func TestDisableBuiltinRejectsUnknownAndRegisteredTools(t *testing.T) {
	reg := New(config.NewLogger(config.LevelError))
	if err := reg.DisableBuiltin("missing"); err == nil {
		t.Fatal("unknown builtin was disabled")
	}
	registerTestTool(t, reg, "test.registered")
	if err := reg.DisableBuiltin("test.registered"); err == nil {
		t.Fatal("builtin was disabled after handler registration")
	}
}
