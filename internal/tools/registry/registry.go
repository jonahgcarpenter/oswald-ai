package registry

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
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
	Enum        []string
}

const (
	// ToolSourceBuiltin identifies tools defined locally in data/tools.
	ToolSourceBuiltin = "builtin"

	// ToolSourceMCP identifies tools discovered from connected MCP servers.
	ToolSourceMCP = "mcp"
)

// Spec holds the fully parsed definition from a single tool markdown file.
// Name and Description are sent to the model via the LLM tool schema.
type Spec struct {
	Name        string
	Description string
	Source      string
	Server      string
	Parameters  []ParamSpec
}

// CatalogEntry is a registry tool definition annotated with its source.
type CatalogEntry struct {
	Name        string
	Description string
	Source      string
	Server      string
	Parameters  []ParamSpec
}

// ToolVisibility controls which non-default tools are sent to the model.
type ToolVisibility struct {
	ExposedMCPTools map[string]bool
}

// Registry maps tool names to their parsed Spec and registered Handler.
// Load tool definitions from a directory of markdown files, then register a Go
// handler for each tool before passing the registry to the agent.
type Registry struct {
	specs    map[string]Spec
	handlers map[string]Handler
	log      *config.Logger
}

// New creates an empty Registry. Call LoadFromDirectory to populate
// it with tool definitions, then RegisterHandler for each tool that needs execution.
func New(log *config.Logger) *Registry {
	return &Registry{
		specs:    make(map[string]Spec),
		handlers: make(map[string]Handler),
		log:      log,
	}
}

// NewFromDirectory creates a Registry and loads tool definitions from dir.
func NewFromDirectory(dir string, log *config.Logger) (*Registry, error) {
	registry := New(log)
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
			r.log.Warn("tool.registry.definition_read_failed", "failed to read tool definition", config.F("file", path), config.F("status", "error"), config.ErrorField(err))
			continue
		}

		spec, err := parseToolMarkdown(string(data))
		if err != nil {
			r.log.Warn("tool.registry.definition_parse_failed", "failed to parse tool definition", config.F("file", path), config.F("status", "error"), config.ErrorField(err))
			continue
		}

		spec.Source = ToolSourceBuiltin

		r.specs[spec.Name] = spec
		r.log.Debug("tool.registry.definition_loaded", "loaded tool definition", config.F("tool_name", spec.Name), config.F("file", entry.Name()))
		loaded++
	}

	if loaded == 0 {
		r.log.Warn("tool.registry.empty", "no tool definitions found", config.F("path", dir), config.F("status", "degraded"))
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
	r.log.Debug("tool.registry.handler_registered", "registered tool handler", config.F("tool_name", name))
	return nil
}

// RegisterSpec adds a tool spec that was discovered programmatically rather than
// loaded from markdown.
func (r *Registry) RegisterSpec(spec Spec) error {
	if spec.Name == "" {
		return fmt.Errorf("cannot register tool spec with empty name")
	}
	if spec.Source == "" {
		spec.Source = ToolSourceBuiltin
	}
	if _, ok := r.specs[spec.Name]; ok {
		return fmt.Errorf("tool spec %q is already registered", spec.Name)
	}
	r.specs[spec.Name] = spec
	r.log.Debug("tool.registry.definition_registered", "registered tool definition", config.F("tool_name", spec.Name), config.F("source", spec.Source), config.F("server", spec.Server))
	return nil
}

// RegisterTool registers a programmatically discovered tool and its handler in a
// single step.
func (r *Registry) RegisterTool(spec Spec, handler Handler) error {
	if err := r.RegisterSpec(spec); err != nil {
		return err
	}
	return r.RegisterHandler(spec.Name, handler)
}

// LLMTools converts builtin Specs into the []llm.Tool slice passed to
// ChatRequest.Tools. MCP specs are hidden until request-local discovery exposes them.
func (r *Registry) LLMTools() []llm.Tool {
	return r.LLMToolsForVisibility(ToolVisibility{})
}

// LLMToolsForVisibility converts loaded Specs into the []llm.Tool slice passed to
// ChatRequest.Tools. Builtin tools are always included. MCP tools are included
// only when explicitly named by the active request's visibility state.
func (r *Registry) LLMToolsForVisibility(visibility ToolVisibility) []llm.Tool {
	tools := make([]llm.Tool, 0, len(r.specs))
	for _, spec := range r.orderedSpecs() {
		if spec.Source == ToolSourceMCP && !visibility.ExposedMCPTools[spec.Name] {
			continue
		}
		props := make(map[string]llm.ToolParameterProperty, len(spec.Parameters))
		required := []string{}
		for _, p := range spec.Parameters {
			props[p.Name] = llm.ToolParameterProperty{
				Type:        p.Type,
				Description: p.Description,
				Enum:        p.Enum,
			}
			if p.Required {
				required = append(required, p.Name)
			}
		}
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolDefinition{
				Name:        spec.Name,
				Description: toolDescription(spec),
				Parameters: llm.ToolParameters{
					Type:       "object",
					Properties: props,
					Required:   required,
				},
			},
		})
	}
	return tools
}

// BuiltinCatalog returns builtin tool definitions in stable order.
func (r *Registry) BuiltinCatalog() []CatalogEntry {
	entries := make([]CatalogEntry, 0)
	for _, spec := range r.orderedSpecs() {
		if spec.Source != ToolSourceBuiltin {
			continue
		}
		entries = append(entries, catalogEntry(spec))
	}
	return entries
}

// CatalogBySource returns tool definitions for source in stable order.
func (r *Registry) CatalogBySource(source string) []CatalogEntry {
	entries := make([]CatalogEntry, 0)
	for _, spec := range r.orderedSpecs() {
		if spec.Source != source {
			continue
		}
		entries = append(entries, catalogEntry(spec))
	}
	return entries
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

func (r *Registry) orderedSpecs() []Spec {
	specs := make([]Spec, 0, len(r.specs))
	for _, spec := range r.specs {
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool {
		left := specSortKey(specs[i])
		right := specSortKey(specs[j])
		if left != right {
			return left < right
		}
		if specs[i].Server != specs[j].Server {
			return specs[i].Server < specs[j].Server
		}
		return specs[i].Name < specs[j].Name
	})
	return specs
}

func specSortKey(spec Spec) int {
	switch spec.Source {
	case ToolSourceBuiltin:
		return 0
	case ToolSourceMCP:
		return 1
	default:
		return 2
	}
}

func toolDescription(spec Spec) string {
	description := strings.TrimSpace(spec.Description)
	if spec.Source != ToolSourceMCP {
		return description
	}
	serverLabel := displayServerName(spec.Server)
	prefix := serverLabel + " MCP tool:"
	if description == "" {
		return prefix
	}
	if strings.HasPrefix(strings.ToLower(description), strings.ToLower(prefix)) {
		return description
	}
	return prefix + " " + description
}

func catalogEntry(spec Spec) CatalogEntry {
	return CatalogEntry{
		Name:        spec.Name,
		Description: spec.Description,
		Source:      spec.Source,
		Server:      spec.Server,
		Parameters:  append([]ParamSpec(nil), spec.Parameters...),
	}
}

func displayServerName(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return "Unknown"
	}
	parts := strings.FieldsFunc(server, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.'
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
	}
	return strings.Join(parts, " ")
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
// Zero-argument tools are allowed and return an empty parameter slice.
func parseParameterTable(section, toolName string) ([]ParamSpec, error) {
	var params []ParamSpec
	hasTableRow := false

	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)

		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}
		hasTableRow = true

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

	if !hasTableRow {
		return nil, fmt.Errorf("tool %q: parameter table has no valid rows", toolName)
	}

	return params, nil
}
