package mcp

import (
	"context"
	"time"
)

const (
	// ScopeGlobal identifies MCP servers available to every user.
	ScopeGlobal = "global"
	// ScopeUser identifies MCP servers available only to one canonical user.
	ScopeUser = "user"

	// TransportStreamableHTTP identifies MCP streamable HTTP transport.
	TransportStreamableHTTP = "streamable_http"
	// TransportSSE is reserved for HTTP SSE MCP transport.
	TransportSSE = "sse"

	serverStatusConnected    = "connected"
	serverStatusError        = "error"
	serverStatusDisabled     = "disabled"
	serverStatusNotConnected = "not_connected"
)

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
	Scope       string
	OwnerUserID string
	RemoteName  string
	Parameters  []ParamSpec
	Handler     Handler
}

// ServerInfo describes a configured MCP server and its current availability.
type ServerInfo struct {
	Name        string
	Description string
	Scope       string
	OwnerUserID string
	Status      string
	ToolCount   int
	Reason      string
}

// ServerConfig is a decrypted MCP server configuration loaded from storage.
type ServerConfig struct {
	ID          string
	Scope       string
	OwnerUserID string
	Name        string
	Type        string
	Transport   string
	URL         string
	Headers     map[string]string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type storedServerConfig struct {
	ID                string
	Scope             string
	OwnerUserID       string
	Name              string
	Type              string
	Transport         string
	URLCiphertext     string
	URLHostHash       string
	HeadersCiphertext string
	Enabled           bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type server struct {
	config ServerConfig
	tools  []ToolSpec
	close  func() error
	reason string
}
