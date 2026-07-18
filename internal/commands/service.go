package commands

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrDuplicateCommand is returned when two handlers register the same command name.
	ErrDuplicateCommand = errors.New("duplicate command")
	// ErrDuplicateAlias is returned when two handlers register the same alias.
	ErrDuplicateAlias = errors.New("duplicate command alias")
)

// Service parses and dispatches slash commands.
type Service struct {
	handlers    map[string]Handler
	aliases     map[string]string
	definitions map[string]Definition
}

// NewService creates a command service from concrete command handlers.
func NewService(handlers ...Handler) (*Service, error) {
	commands := make([]Command, 0, len(handlers))
	for _, handler := range handlers {
		commands = append(commands, Command{Handler: handler})
	}
	return NewServiceWithCommands(commands...)
}

// NewServiceWithCommands creates a command service from command registrations.
func NewServiceWithCommands(commands ...Command) (*Service, error) {
	service := &Service{
		handlers:    make(map[string]Handler, len(commands)),
		aliases:     make(map[string]string),
		definitions: make(map[string]Definition, len(commands)),
	}

	for _, command := range commands {
		handler := command.Handler
		if handler == nil {
			continue
		}
		definition := normalizeDefinition(handler.Definition())
		if definition.Name == "" {
			return nil, fmt.Errorf("command definition missing name")
		}
		if _, exists := service.handlers[definition.Name]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateCommand, definition.Name)
		}
		if _, exists := service.aliases[definition.Name]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateCommand, definition.Name)
		}
		handler = applyMiddleware(handler, command.Middleware)
		service.handlers[definition.Name] = handler
		service.definitions[definition.Name] = definition
		for _, alias := range definition.Aliases {
			if alias == "" {
				continue
			}
			if _, exists := service.handlers[alias]; exists {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateAlias, alias)
			}
			if _, exists := service.aliases[alias]; exists {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateAlias, alias)
			}
			service.aliases[alias] = definition.Name
		}
	}

	return service, nil
}

// Execute parses and runs a command attempt. Unknown attempts return a user-facing result.
func (s *Service) Execute(ctx context.Context, req Request) (Result, error) {
	if !req.Principal.Valid() {
		return Result{}, fmt.Errorf("command request has no valid principal")
	}
	parsed, ok := Parse(req.Raw)
	if !ok || parsed.Name == "" {
		return Result{Text: "Unknown command: /"}, nil
	}

	name := parsed.Name
	canonicalName := name
	if target, ok := s.aliases[name]; ok {
		canonicalName = target
	}
	handler, ok := s.handlers[canonicalName]
	if !ok {
		return Result{Text: "Unknown command: /" + name}, nil
	}

	req.Raw = parsed.Raw
	req.Name = canonicalName
	req.Args = parsed.Args
	req.ArgsText = parsed.ArgsText
	return handler.Execute(ctx, req)
}

// Definitions returns all registered command definitions.
func (s *Service) Definitions() []Definition {
	definitions := make([]Definition, 0, len(s.definitions))
	for _, definition := range s.definitions {
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(i, j int) bool {
		return definitions[i].Name < definitions[j].Name
	})
	return definitions
}

func applyMiddleware(handler Handler, middleware []Middleware) Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		if middleware[i] != nil {
			handler = middleware[i](handler)
		}
	}
	return handler
}

func normalizeDefinition(definition Definition) Definition {
	definition.Name = normalizeName(definition.Name)
	aliases := make([]string, 0, len(definition.Aliases))
	seen := make(map[string]bool, len(definition.Aliases))
	for _, alias := range definition.Aliases {
		alias = normalizeName(alias)
		if alias == "" || alias == definition.Name || seen[alias] {
			continue
		}
		aliases = append(aliases, alias)
		seen[alias] = true
	}
	definition.Aliases = aliases
	definition.Summary = strings.TrimSpace(definition.Summary)
	definition.Usage = strings.TrimSpace(definition.Usage)
	return definition
}

func normalizeName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}
