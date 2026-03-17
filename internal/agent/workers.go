package agent

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkerAgent describes a single model entry: its category identifier, Ollama model name,
// and the system prompt injected into every call for that model.
type WorkerAgent struct {
	Category     string `yaml:"category"`
	Model        string `yaml:"model"`
	SystemPrompt string `yaml:"system_prompt"`
}

// workerFile is the top-level structure of workers.yaml.
type workerFile struct {
	Workers []WorkerAgent `yaml:"workers"`
}

// LoadWorkers reads the YAML file at path and returns the list of worker agents.
// Returns an error if the file is missing, unparseable, or defines no workers.
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
