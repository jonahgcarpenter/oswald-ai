package agent

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkerAgent describes a single expert agent available for routing.
type WorkerAgent struct {
	Category     string `yaml:"category"`
	Description  string `yaml:"description"`
	ModelEnv     string `yaml:"model_env"`
	ModelDefault string `yaml:"model_default"`
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

// ResolveModel returns the model name for this worker, preferring the
// environment variable override over the default.
func (w *WorkerAgent) ResolveModel() string {
	if w.ModelEnv != "" {
		if v, ok := os.LookupEnv(w.ModelEnv); ok && v != "" {
			return v
		}
	}
	return w.ModelDefault
}

// FindWorker returns the WorkerAgent whose Category matches the given string
// (case-insensitive). Returns nil if no match is found.
func FindWorker(workers []WorkerAgent, category string) *WorkerAgent {
	upper := strings.ToUpper(category)
	for i := range workers {
		if strings.ToUpper(workers[i].Category) == upper {
			return &workers[i]
		}
	}
	return nil
}

// BuildTriagePrompt generates the triage system prompt dynamically from the
// registered workers. Workers are numbered in priority order (1 = highest).
// The prompt is kept intentionally terse to work well with small router models.
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
