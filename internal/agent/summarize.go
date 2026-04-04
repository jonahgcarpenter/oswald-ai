package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// maxSummaryChars caps the rolling summary length to keep it from consuming
// too much of the context budget across many pruning cycles.
const maxSummaryChars = 2000

// OllamaSummarizer generates rolling conversation summaries using the same
// Ollama model as the main agent. It is invoked synchronously when retention
// or context-budget pruning removes turns that would otherwise be lost.
type OllamaSummarizer struct {
	client ollama.Chatter
	model  string
	log    *config.Logger
}

// NewOllamaSummarizer creates a new OllamaSummarizer backed by the given Ollama
// chat client and model. The summarizer uses no tools and no streaming.
func NewOllamaSummarizer(client ollama.Chatter, model string, log *config.Logger) *OllamaSummarizer {
	return &OllamaSummarizer{
		client: client,
		model:  model,
		log:    log,
	}
}

// Summarize distills the given conversation turns into a concise rolling summary.
// If existingSummary is non-empty it is merged with the new material so the
// returned string captures the full pruned history, not just the latest batch.
// The result is capped at maxSummaryChars to prevent unbounded growth.
func (s *OllamaSummarizer) Summarize(ctx context.Context, existingSummary string, turns []memory.Turn) (string, error) {
	if len(turns) == 0 {
		return existingSummary, nil
	}

	// Build the conversation transcript from the pruned turns.
	var transcript strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&transcript, "User: %s\nAssistant: %s\n\n", t.User.Content, t.Assistant.Content)
	}

	// Build the summarization prompt.
	var promptParts []string
	if existingSummary != "" {
		promptParts = append(promptParts, "Existing summary:\n"+existingSummary)
	}
	promptParts = append(promptParts, "New conversation to incorporate:\n"+transcript.String())
	promptParts = append(promptParts,
		"Write a concise summary (2-4 sentences) that merges the existing summary with the "+
			"new conversation. Preserve key facts, names, decisions, and ongoing topics. "+
			"Do not invent or infer anything not stated. Output only the summary text, no preamble.")

	summaryPrompt := strings.Join(promptParts, "\n\n")

	req := ollama.ChatRequest{
		Model: s.model,
		Messages: []ollama.ChatMessage{
			{Role: "user", Content: summaryPrompt},
		},
		Stream: false,
	}

	resp, err := s.client.Chat(ctx, req, nil)
	if err != nil {
		return existingSummary, fmt.Errorf("summarizer: ollama call failed: %w", err)
	}

	result := strings.TrimSpace(resp.Message.Content)
	if result == "" {
		s.log.Warn("Summarizer: model returned empty summary; retaining existing")
		return existingSummary, nil
	}

	// Cap the summary length to protect the context budget.
	if len(result) > maxSummaryChars {
		runes := []rune(result)
		result = string(runes[:maxSummaryChars])
	}

	s.log.Debug("Summarizer: generated %d-char summary from %d pruned turn(s)", len(result), len(turns))
	return result, nil
}
