package mcp

import "context"

// Handler executes an MCP-backed tool call.
type Handler func(ctx context.Context, arguments map[string]interface{}) (string, error)

// ParamSpec describes a single MCP tool parameter after schema normalization.
type ParamSpec struct {
	Name        string
	Type        string
	Required    bool
	Description string
	Enum        []string
}

// ToolSpec describes a discovered MCP tool in the format the local registry needs.
type ToolSpec struct {
	Name        string
	Description string
	Server      string
	Parameters  []ParamSpec
	Handler     Handler
}

// ServerInfo describes a configured MCP server and its current availability.
type ServerInfo struct {
	Name        string
	Description string
	Status      string
	ToolCount   int
	Reason      string
}

type server struct {
	name  string
	tools []ToolSpec
	close func() error
}
