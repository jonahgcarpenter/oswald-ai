package agent

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// WorkerAgent describes a single expert agent available for routing.
type WorkerAgent struct {
	Category     string `yaml:"category"`
	Description  string `yaml:"description"`
	Model        string `yaml:"model"`
	SystemPrompt string `yaml:"system_prompt"`
}

// workerFile is the top-level structure of workers.yaml.
type workerFile struct {
	Workers []WorkerAgent `yaml:"workers"`
}

// LoadWorkers reads the YAML file at path and resolves each worker's model
// from its ModelEnv environment variable, falling back to ModelDefault.
func LoadWorkers(path string) ([]WorkerAgent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read workers config %q: %w", path, err)
	}

	var wf workerFile
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("failed to parse workers config %q: %w", path, err)
	}

	if len(wf.Workers) == 0 {
		return nil, fmt.Errorf("workers config %q defines no workers", path)
	}

	return wf.Workers, nil
}

// ResolveModel returns the model name for this worker.
func (w *WorkerAgent) ResolveModel() string {
	return w.Model
}

// FindWorker returns the WorkerAgent whose Category matches the given string (case-insensitive).
// Returns nil if no match is found.
func FindWorker(workers []WorkerAgent, category string) *WorkerAgent {
	upper := strings.ToUpper(category)
	for i := range workers {
		if strings.ToUpper(workers[i].Category) == upper {
			return &workers[i]
		}
	}
	return nil
}

// triageToolName returns the tool name used to route to the given worker category.
// Tool names use underscores since many LLMs reject hyphens in function names.
func triageToolName(category string) string {
	return "route_to_" + strings.ToUpper(category)
}

// CategoryFromToolName extracts the worker category from a triage tool name.
// Expects the format "route_to_CATEGORY". Returns an empty string if the name does not match.
func CategoryFromToolName(toolName string) string {
	const prefix = "route_to_"
	upper := strings.ToUpper(toolName)
	if strings.HasPrefix(upper, strings.ToUpper(prefix)) {
		return upper[len(prefix):]
	}
	return ""
}

// BuildTriageTools generates one llm.Tool per registered worker. Each tool represents
// a routing option; the router model calls exactly one to communicate its decision.
// This tool-calling approach is more reliable than parsing generated text.
func BuildTriageTools(workers []WorkerAgent) []llm.Tool {
	tools := make([]llm.Tool, len(workers))
	for i, w := range workers {
		desc := strings.TrimSpace(w.Description)
		tools[i] = llm.Tool{
			Type: "function",
			Function: llm.ToolDefinition{
				Name:        triageToolName(w.Category),
				Description: fmt.Sprintf("Route to this worker for: %s", desc),
				Parameters: llm.ToolParameters{
					Type: "object",
					Properties: map[string]llm.ToolParameterProperty{
						"reason": {
							Type:        "string",
							Description: "Brief reason for this routing decision (10 words or fewer)",
						},
					},
					Required: []string{"reason"},
				},
			},
		}
	}
	return tools
}

// BuildTriagePrompt generates the triage system prompt dynamically from the
// registered workers. Workers are numbered in priority order (1 = highest).
// The prompt is kept intentionally terse to work well with small router models.
// Deprecated: Use BuildTriageTools for tool-calling based routing instead.
func BuildTriagePrompt(workers []WorkerAgent) string {
	var sb strings.Builder

	// Build the allowed category list for the output rule
	cats := make([]string, len(workers))
	for i, w := range workers {
		cats[i] = fmt.Sprintf("%q", w.Category)
	}

	sb.WriteString("You are a request router. Classify the user message into exactly one category.\n\n")
	sb.WriteString("CATEGORIES (check in order — use the first one that matches):\n")

	for i, w := range workers {
		desc := strings.TrimSpace(w.Description)
		fmt.Fprintf(&sb, "%d. %s — %s\n", i+1, w.Category, desc)
	}

	sb.WriteString("\nRULES:\n")
	fmt.Fprintf(&sb, "- category MUST be one of: %s\n", strings.Join(cats, ", "))
	sb.WriteString("- reason must be 10 words or fewer\n")
	sb.WriteString("- output ONLY valid JSON, no other text\n\n")
	sb.WriteString(`Output format: {"category": "...", "reason": "..."}`)

	return sb.String()
}
