package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// Handler is the execution function for a single tool.
// It receives the model's tool call arguments and returns the text content to
// inject as a tool response message. ctx is propagated for timeout cancellation.
type Handler func(ctx context.Context, arguments map[string]interface{}) (string, error)

// ParamSpec describes one parameter row parsed from the markdown table.
type ParamSpec struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// Spec holds the fully parsed definition from a single tool markdown file.
// Name and Description are sent to the model via the ollama.Tool schema.
type Spec struct {
	Name        string
	Description string
	Parameters  []ParamSpec
}

// Registry maps tool names to their parsed Spec and registered Handler.
// Load tool definitions from a directory of markdown files, then register a Go
// handler for each tool before passing the registry to the agent.
type Registry struct {
	specs    map[string]Spec
	handlers map[string]Handler
	log      *config.Logger
}

// NewRegistry creates an empty Registry. Call LoadFromDirectory to populate
// it with tool definitions, then RegisterHandler for each tool that needs execution.
func NewRegistry(log *config.Logger) *Registry {
	return &Registry{
		specs:    make(map[string]Spec),
		handlers: make(map[string]Handler),
		log:      log,
	}
}

// NewRegistryFromDirectory creates a Registry and loads tool definitions from dir.
func NewRegistryFromDirectory(dir string, log *config.Logger) (*Registry, error) {
	registry := NewRegistry(log)
	if err := registry.LoadFromDirectory(dir); err != nil {
		return nil, fmt.Errorf("failed to load tool definitions: %w", err)
	}
	return registry, nil
}

// LoadFromDirectory reads all *.md files in dir and parses each as a tool definition.
// Files that fail to parse are logged and skipped; the method only returns an error
// if the directory itself cannot be read.
func (r *Registry) LoadFromDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read tools directory %q: %w", dir, err)
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			r.log.Warn("Tools: failed to read %q: %v", path, err)
			continue
		}

		spec, err := parseToolMarkdown(string(data))
		if err != nil {
			r.log.Warn("Tools: failed to parse %q: %v", path, err)
			continue
		}

		r.specs[spec.Name] = spec
		r.log.Debug("Tools: loaded %q from %s", spec.Name, entry.Name())
		loaded++
	}

	if loaded == 0 {
		r.log.Warn("Tools: no tool definitions found in %q", dir)
	}

	return nil
}

// RegisterHandler associates a Handler with a tool name.
// Returns an error if the name does not match any loaded tool spec, to catch
// typos and orphaned handlers early at startup.
func (r *Registry) RegisterHandler(name string, handler Handler) error {
	if _, ok := r.specs[name]; !ok {
		return fmt.Errorf("cannot register handler for %q: no tool spec loaded with that name", name)
	}
	r.handlers[name] = handler
	r.log.Debug("Tools: registered handler for %q", name)
	return nil
}

// OllamaTools converts all loaded Specs into the []ollama.Tool slice
// passed to ChatRequest.Tools. All loaded specs are included regardless of
// whether a handler has been registered.
func (r *Registry) OllamaTools() []ollama.Tool {
	tools := make([]ollama.Tool, 0, len(r.specs))
	for _, spec := range r.specs {
		props := make(map[string]ollama.ToolParameterProperty, len(spec.Parameters))
		required := []string{}
		for _, p := range spec.Parameters {
			props[p.Name] = ollama.ToolParameterProperty{
				Type:        p.Type,
				Description: p.Description,
			}
			if p.Required {
				required = append(required, p.Name)
			}
		}
		tools = append(tools, ollama.Tool{
			Type: "function",
			Function: ollama.ToolDefinition{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters: ollama.ToolParameters{
					Type:       "object",
					Properties: props,
					Required:   required,
				},
			},
		})
	}
	return tools
}

// Execute calls the registered handler for the named tool with the given arguments.
// Returns an error if no handler is registered for the tool name.
func (r *Registry) Execute(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return "", fmt.Errorf("no handler registered for tool %q", name)
	}
	return handler(ctx, args)
}

// HasHandler returns true if a handler has been registered for the given tool name.
func (r *Registry) HasHandler(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// Count returns the number of tool specs loaded in the registry.
func (r *Registry) Count() int {
	return len(r.specs)
}

// Names returns the loaded tool names in stable sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.specs))
	for name := range r.specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parseToolMarkdown parses a tool definition from a markdown string.
//
// Expected format:
//
//	# tool_name
//
//	## Description
//
//	Full description text (may include markdown formatting, lists, etc.)
//
//	## Parameters
//
//	| Name | Type | Required | Description |
//	|------|------|----------|-------------|
//	| param | string | yes | Description of the parameter |
func parseToolMarkdown(content string) (Spec, error) {
	var spec Spec

	lines := strings.Split(content, "\n")

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			spec.Name = strings.TrimSpace(trimmed[2:])
			break
		}
	}
	if spec.Name == "" {
		return spec, fmt.Errorf("missing # heading for tool name")
	}

	sections := splitMarkdownSections(content)

	descSection, hasDesc := sections["Description"]
	if !hasDesc || strings.TrimSpace(descSection) == "" {
		return spec, fmt.Errorf("tool %q: missing ## Description section", spec.Name)
	}
	spec.Description = strings.TrimSpace(descSection)

	paramSection, hasParams := sections["Parameters"]
	if !hasParams || strings.TrimSpace(paramSection) == "" {
		return spec, fmt.Errorf("tool %q: missing ## Parameters section", spec.Name)
	}

	params, err := parseParameterTable(paramSection, spec.Name)
	if err != nil {
		return spec, err
	}
	spec.Parameters = params

	return spec, nil
}

// splitMarkdownSections splits the content after the H1 heading into named
// sections keyed by their ## heading text. The value is the raw content between
// that heading and the next ## heading (or end of file).
func splitMarkdownSections(content string) map[string]string {
	sections := make(map[string]string)
	lines := strings.Split(content, "\n")

	currentSection := ""
	var sb strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			continue
		}

		if strings.HasPrefix(trimmed, "## ") {
			if currentSection != "" {
				sections[currentSection] = sb.String()
				sb.Reset()
			}
			currentSection = strings.TrimSpace(trimmed[3:])
			continue
		}

		if currentSection != "" {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}

	if currentSection != "" {
		sections[currentSection] = sb.String()
	}

	return sections
}

// parseParameterTable parses a markdown table of tool parameters.
// Expected columns (in order): Name, Type, Required, Description.
// Skips the header row and any separator rows (containing only dashes and pipes).
func parseParameterTable(section, toolName string) ([]ParamSpec, error) {
	var params []ParamSpec

	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)

		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}

		inner := strings.TrimPrefix(line, "|")
		inner = strings.TrimSuffix(inner, "|")
		cells := strings.Split(inner, "|")

		if len(cells) < 4 {
			continue
		}

		name := strings.TrimSpace(cells[0])
		typ := strings.TrimSpace(cells[1])
		reqStr := strings.TrimSpace(cells[2])
		desc := strings.TrimSpace(cells[3])

		if name == "Name" || strings.ContainsAny(name, "-") {
			continue
		}
		if name == "" || typ == "" {
			continue
		}

		params = append(params, ParamSpec{
			Name:        name,
			Type:        typ,
			Required:    strings.EqualFold(reqStr, "yes"),
			Description: desc,
		})
	}

	if len(params) == 0 {
		return nil, fmt.Errorf("tool %q: parameter table has no valid rows", toolName)
	}

	return params, nil
}
