package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

// maxSummaryChars caps compacted history length to keep it from consuming too
// much of the context budget across many compaction cycles.
const maxSummaryChars = 2000

// Summarizer generates compacted conversation summaries using the same
// model as the main agent. It is invoked synchronously when prompt
// budget pressure requires older retained turns to be collapsed into a single
// replacement turn pair.
type Summarizer struct {
	client llm.Chatter
	model  string
	log    *config.Logger
}

// NewSummarizer creates a new Summarizer backed by the given chat client and model.
// The summarizer uses no tools and no streaming.
func NewSummarizer(client llm.Chatter, model string, log *config.Logger) *Summarizer {
	return &Summarizer{
		client: client,
		model:  model,
		log:    log,
	}
}

// Summarize distills the given conversation turns into a concise compacted
// history summary. The result is capped at maxSummaryChars to prevent
// unbounded growth across repeated compaction cycles.
func (s *Summarizer) Summarize(ctx context.Context, turns []memory.Turn) (string, error) {
	if len(turns) == 0 {
		return "", nil
	}

	// Build the conversation transcript from the turns being compacted.
	var transcript strings.Builder
	for _, t := range turns {
		fmt.Fprintf(&transcript, "User: %s\nAssistant: %s\n\n", t.User.Content, t.Assistant.Content)
	}

	summaryPrompt := strings.Join([]string{
		"You are compacting earlier conversation history into a shorter replacement for memory retention.",
		"Conversation to compact:\n" + transcript.String(),
		"Write a concise 2-4 sentence summary that preserves key facts, names, decisions, requests, and ongoing topics. Do not invent or infer anything not stated. Output only the summary text, no preamble.",
	}, "\n\n")

	req := llm.ChatRequest{
		Model: s.model,
		Messages: []llm.ChatMessage{
			{Role: "user", Content: summaryPrompt},
		},
		Stream: false,
	}

	resp, err := s.client.Chat(ctx, req, nil)
	if err != nil {
		return "", fmt.Errorf("summarizer: model call failed: %w", err)
	}

	result := strings.TrimSpace(resp.Message.Content)
	if result == "" {
		return "", fmt.Errorf("summarizer: model returned empty summary")
	}

	// Cap the summary length to protect the context budget.
	if len(result) > maxSummaryChars {
		runes := []rune(result)
		result = string(runes[:maxSummaryChars])
	}

	meta := requestctx.MetadataFromContext(ctx)
	s.log.Agent("agent.summarizer", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model).Debug(
		"agent.summarizer.generated",
		"generated history summary",
		config.F("summary_chars", len(result)),
		config.F("turn_pair_count", len(turns)),
	)
	return result, nil
}
