package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	globalmemorycommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/globalmemory"
	mcpcommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/mcp"
	memoriescommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/memories"
	sessioncommands "github.com/jonahgcarpenter/oswald-ai/internal/commands/session"
	"github.com/jonahgcarpenter/oswald-ai/internal/commands/usermanagement"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	mcpmanager "github.com/jonahgcarpenter/oswald-ai/internal/mcp"
	globalmemorystore "github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/globalmemory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// MCPDeps contains optional dependencies for MCP management commands.
type MCPDeps struct {
	Store   *mcpmanager.Store
	Manager *mcpmanager.Manager
}

// NewService creates the application command service with all built-in commands.
func NewService(users *accountlinking.Service, memory *usermemory.Store, optionalMCP ...MCPDeps) (*commands.Service, error) {
	return NewServiceWithGlobalMemory(users, memory, nil, nil, nil, optionalMCP...)
}

// NewServiceWithGlobalMemory creates the full command service including global-memory administration.
func NewServiceWithGlobalMemory(users *accountlinking.Service, memory *usermemory.Store, globalMemory *globalmemorystore.Store, log *config.Logger, bootstrap commands.Handler, optionalMCP ...MCPDeps) (*commands.Service, error) {
	if memory == nil {
		return nil, fmt.Errorf("user memory store is required for built-in commands")
	}
	help := &helpHandler{auth: users}
	registrations := []commands.Command{{Handler: help}, {Handler: sessioncommands.New(memory)}}
	if users != nil {
		registrations = append(registrations, commands.Command{Handler: memoriescommands.New(users, memory)})
	}
	if len(optionalMCP) > 0 && optionalMCP[0].Store != nil && optionalMCP[0].Manager != nil {
		registrations = append(registrations, commands.Command{Handler: mcpcommands.New(optionalMCP[0].Store, optionalMCP[0].Manager, users)})
	}
	if bootstrap != nil {
		registrations = append(registrations, commands.Command{Handler: bootstrap})
	}
	for _, handler := range accountlinking.New(users) {
		registrations = append(registrations, commands.Command{Handler: handler})
	}
	for _, handler := range usermanagement.New(users) {
		registrations = append(registrations, commands.Command{Handler: handler, Middleware: []commands.Middleware{commands.RequireAdmin(users)}})
	}
	if globalMemory != nil {
		registrations = append(registrations, commands.Command{Handler: globalmemorycommands.New(globalMemory, log), Middleware: []commands.Middleware{commands.RequireAdmin(users)}})
	}
	service, err := commands.NewServiceWithCommands(registrations...)
	if err != nil {
		return nil, err
	}
	help.commands = service
	return service, nil
}

type helpHandler struct {
	commands *commands.Service
	auth     commands.Authorizer
}

func (h helpHandler) Definition() commands.Definition {
	return commands.Definition{Name: "help", Summary: "List commands or show usage for one command.", Usage: "/help [command]"}
}

func (h helpHandler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	definitions, err := h.visibleDefinitions(ctx, req.Principal.CanonicalUserID)
	if err != nil {
		return commands.Result{}, err
	}
	if len(req.Args) > 0 {
		want := strings.TrimPrefix(strings.TrimSpace(req.Args[0]), "/")
		for _, definition := range definitions {
			if definition.Name == want {
				return commands.Result{Text: renderHelpFor(definition)}, nil
			}
			for _, alias := range definition.Aliases {
				if alias == want {
					return commands.Result{Text: renderHelpFor(definition)}, nil
				}
			}
		}
		return commands.Result{Text: "Unknown command: /" + want}, nil
	}

	lines := make([]string, 0, len(definitions)+1)
	lines = append(lines, "Commands:")
	for _, definition := range definitions {
		line := "/" + definition.Name
		if definition.Summary != "" {
			line += " - " + definition.Summary
		}
		lines = append(lines, line)
	}
	return commands.Result{Text: strings.Join(lines, "\n")}, nil
}

func (h helpHandler) visibleDefinitions(_ context.Context, userID string) ([]commands.Definition, error) {
	definitions := h.commands.Definitions()
	if h.auth == nil {
		return filterAdminDefinitions(definitions, false), nil
	}
	isAdmin, err := h.auth.IsAdmin(userID)
	if err != nil {
		return nil, err
	}
	return filterAdminDefinitions(definitions, isAdmin), nil
}

func filterAdminDefinitions(definitions []commands.Definition, includeAdmin bool) []commands.Definition {
	filtered := make([]commands.Definition, 0, len(definitions))
	for _, definition := range definitions {
		if definition.AdminOnly && !includeAdmin {
			continue
		}
		filtered = append(filtered, definition)
	}
	return filtered
}

func renderHelpFor(definition commands.Definition) string {
	lines := []string{"/" + definition.Name}
	if definition.Summary != "" {
		lines = append(lines, definition.Summary)
	}
	if definition.Usage != "" {
		lines = append(lines, "Use: "+definition.Usage)
	}
	return strings.Join(lines, "\n")
}
