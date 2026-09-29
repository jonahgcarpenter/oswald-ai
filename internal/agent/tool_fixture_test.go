package agent

import (
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/registry"
)

type testToolParam struct {
	Name        string
	Type        string
	Required    bool
	Description string
	Enum        []string
}

type testToolSpec struct {
	Name        string
	Description string
	Parameters  []testToolParam
	Schema      *llm.ToolParameters
}

func registerTestTool(t *testing.T, reg *registry.Registry, spec testToolSpec, policy governance.ToolPolicy, handler registry.Handler) error {
	t.Helper()
	schema := llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{}}
	if spec.Schema != nil {
		schema = *spec.Schema
	} else {
		for _, param := range spec.Parameters {
			schema.Properties[param.Name] = llm.ToolParameterProperty{Type: param.Type, Description: param.Description, Enum: param.Enum}
			if param.Required {
				schema.Required = append(schema.Required, param.Name)
			}
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]llm.ToolParameterProperty{}
	}
	description := spec.Description
	if description == "" {
		description = spec.Name
	}
	if err := reg.RegisterDefinition(llm.ToolDefinition{Name: spec.Name, Description: description, Parameters: schema}); err != nil {
		return err
	}
	return reg.RegisterHandler(spec.Name, policy, handler)
}
