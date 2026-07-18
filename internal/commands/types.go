package commands

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
)

// Definition describes a command registered with the command service.
type Definition struct {
	Name      string
	Aliases   []string
	Summary   string
	Usage     string
	AdminOnly bool
}

// Request is the gateway-neutral command execution context.
type Request struct {
	RequestID   string
	Principal   identity.Principal
	ChatID      string
	SessionKey  string
	DisplayName string

	Raw      string
	Name     string
	Args     []string
	ArgsText string
}

// Result is the user-facing command response.
type Result struct {
	Text string
}

// UsageText renders the standard command usage response.
func UsageText(definition Definition) string {
	if definition.Summary == "" {
		return "Use: " + definition.Usage
	}
	return definition.Summary + "\nUse: " + definition.Usage
}

// Handler executes one registered command.
type Handler interface {
	Definition() Definition
	Execute(context.Context, Request) (Result, error)
}

// HandlerFunc adapts a function to a command handler.
type HandlerFunc struct {
	DefinitionValue Definition
	ExecuteFunc     func(context.Context, Request) (Result, error)
}

// Definition returns the function handler's command metadata.
func (h HandlerFunc) Definition() Definition {
	return h.DefinitionValue
}

// Execute runs the wrapped function.
func (h HandlerFunc) Execute(ctx context.Context, req Request) (Result, error) {
	return h.ExecuteFunc(ctx, req)
}

// Middleware wraps a command handler with cross-cutting behavior.
type Middleware func(Handler) Handler

// Command registers a handler and its middleware with the command service.
type Command struct {
	Handler    Handler
	Middleware []Middleware
}

// Authorizer checks command-level permissions for canonical users.
type Authorizer interface {
	IsAdmin(canonicalUserID string) (bool, error)
}
