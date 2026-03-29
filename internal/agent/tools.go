package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/search"
)

// ToolHandler is the execution function for a single tool.
// It receives the model's tool call arguments and returns the text content to
// inject as a tool response message. ctx is propagated for timeout cancellation.
type ToolHandler func(ctx context.Context, arguments map[string]interface{}) (string, error)

// ToolParamSpec describes one parameter row parsed from the markdown table.
type ToolParamSpec struct {
	Name        string
	Type        string
	Required    bool
	Description string
}

// ToolSpec holds the fully parsed definition from a single tool markdown file.
// Name and Description are sent to the model via the ollama.Tool schema.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  []ToolParamSpec
}

// ToolRegistry maps tool names to their parsed ToolSpec and registered ToolHandler.
// Load tool definitions from a directory of markdown files, then register a Go
// handler for each tool before passing the registry to NewAgent.
type ToolRegistry struct {
	specs    map[string]ToolSpec
	handlers map[string]ToolHandler
	log      *config.Logger
}

// NewToolRegistry creates an empty ToolRegistry. Call LoadFromDirectory to populate
// it with tool definitions, then RegisterHandler for each tool that needs execution.
func NewToolRegistry(log *config.Logger) *ToolRegistry {
	return &ToolRegistry{
		specs:    make(map[string]ToolSpec),
		handlers: make(map[string]ToolHandler),
		log:      log,
	}
}

// LoadFromDirectory reads all *.md files in dir and parses each as a tool definition.
// Files that fail to parse are logged and skipped; the method only returns an error
// if the directory itself cannot be read.
func (r *ToolRegistry) LoadFromDirectory(dir string) error {
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
		r.log.Info("Tools: loaded %q from %s", spec.Name, entry.Name())
		loaded++
	}

	if loaded == 0 {
		r.log.Warn("Tools: no tool definitions found in %q", dir)
	}

	return nil
}

// RegisterHandler associates a ToolHandler with a tool name.
// Returns an error if the name does not match any loaded tool spec, to catch
// typos and orphaned handlers early at startup.
func (r *ToolRegistry) RegisterHandler(name string, handler ToolHandler) error {
	if _, ok := r.specs[name]; !ok {
		return fmt.Errorf("cannot register handler for %q: no tool spec loaded with that name", name)
	}
	r.handlers[name] = handler
	r.log.Info("Tools: registered handler for %q", name)
	return nil
}

// OllamaTools converts all loaded ToolSpecs into the []ollama.Tool slice
// passed to ChatRequest.Tools. All loaded specs are included regardless of
// whether a handler has been registered.
func (r *ToolRegistry) OllamaTools() []ollama.Tool {
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
func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return "", fmt.Errorf("no handler registered for tool %q", name)
	}
	return handler(ctx, args)
}

// HasHandler returns true if a handler has been registered for the given tool name.
func (r *ToolRegistry) HasHandler(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// Count returns the number of tool specs loaded in the registry.
func (r *ToolRegistry) Count() int {
	return len(r.specs)
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
func parseToolMarkdown(content string) (ToolSpec, error) {
	var spec ToolSpec

	lines := strings.Split(content, "\n")

	// Extract tool name from first H1 heading
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

	// Split into sections by ## headings
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

		// Skip the H1 line
		if strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(trimmed, "## ") {
			continue
		}

		if strings.HasPrefix(trimmed, "## ") {
			// Save previous section if any
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

	// Save last section
	if currentSection != "" {
		sections[currentSection] = sb.String()
	}

	return sections
}

// parseParameterTable parses a markdown table of tool parameters.
// Expected columns (in order): Name, Type, Required, Description.
// Skips the header row and any separator rows (containing only dashes and pipes).
func parseParameterTable(section, toolName string) ([]ToolParamSpec, error) {
	var params []ToolParamSpec

	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)

		// Must be a table row
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}

		// Strip outer pipes and split into cells
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

		// Skip header row (Name/Type/Required/Description) and separator rows (---|---|...)
		if name == "Name" || strings.ContainsAny(name, "-") {
			continue
		}
		if name == "" || typ == "" {
			continue
		}

		params = append(params, ToolParamSpec{
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

// formatSearchResults converts a slice of search results into a plain-text block
// suitable for injection as a tool response message back to the model.
func formatSearchResults(results []search.SearchResult) string {
	if len(results) == 0 {
		return "No results found."
	}
	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content)
	}
	return strings.TrimSpace(sb.String())
}

// NewWebSearchHandler returns a ToolHandler that executes web searches via searcher.
// The result cap (maxResults) limits how many total search results are accumulated
// across all tool calls within a single Process() invocation.
// NOTE: The cap is tracked externally by the agent; this handler is stateless.
func NewWebSearchHandler(searcher search.Searcher, log *config.Logger) ToolHandler {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		query := ""
		if q, ok := args["query"]; ok {
			query, _ = q.(string)
		}

		if query == "" {
			return "", fmt.Errorf("query parameter was empty")
		}

		log.Info("Web search: executing query %q", query)

		results, err := searcher.Search(ctx, query)
		if err != nil {
			return "", fmt.Errorf("search failed: %w", err)
		}

		return formatSearchResults(results), nil
	}
}
