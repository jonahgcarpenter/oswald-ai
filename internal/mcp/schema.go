package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

func schemaToParams(inputSchema any) ([]ParamSpec, error) {
	if inputSchema == nil {
		return nil, nil
	}

	var schema struct {
		Type       any `json:"type"`
		Properties map[string]struct {
			Type        any      `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}

	data, err := json.Marshal(inputSchema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	if schemaType(schema.Type) != "object" {
		return nil, fmt.Errorf("unsupported top-level schema type %q", schemaType(schema.Type))
	}

	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = true
	}

	params := make([]ParamSpec, 0, len(schema.Properties))
	for name, property := range schema.Properties {
		propType := schemaType(property.Type)
		if propType == "" {
			propType = "string"
		}
		if propType != "string" && propType != "number" && propType != "integer" && propType != "boolean" && propType != "object" && propType != "array" {
			return nil, fmt.Errorf("unsupported property type %q for %s", propType, name)
		}
		params = append(params, ParamSpec{
			Name:        name,
			Type:        propType,
			Required:    required[name],
			Description: strings.TrimSpace(property.Description),
			Enum:        property.Enum,
		})
	}

	return params, nil
}

func schemaType(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "null" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}
