package startup

import (
	"context"
	"errors"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

// summarizerTarget is the markdown compaction operation set the holder
// delegates to. *compaction.Summarizer satisfies it; tests substitute fakes.
type summarizerTarget interface {
	Summarize(context.Context, []memory.SessionTurn) (string, *llm.ChatResponse, error)
	Get(string, string, int) string
	Set(string, string, int, string)
}

// summarizerHolder owns the profile's current markdown summarizer so a hot
// model-routing reload can re-seat it without changing the agent or `/compact`
// signatures. Each call snapshots one target, so an in-flight compaction
// finishes on the summarizer it started with.
type summarizerHolder struct {
	mu      sync.RWMutex
	current summarizerTarget
}

// newSummarizerHolder wraps the summarizer built from the startup config.
func newSummarizerHolder(initial summarizerTarget) *summarizerHolder {
	return &summarizerHolder{current: initial}
}

// Rotate re-seats the holder on the summarizer built for a new model or
// transport. A nil target is ignored so a failed construction never clears a
// working summarizer.
func (h *summarizerHolder) Rotate(next summarizerTarget) {
	if h == nil || next == nil {
		return
	}
	h.mu.Lock()
	h.current = next
	h.mu.Unlock()
}

func (h *summarizerHolder) target() summarizerTarget {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.current
}

// Summarize implements agent.MarkdownSummarizer and the `/compact` Summarizer.
func (h *summarizerHolder) Summarize(ctx context.Context, turns []memory.SessionTurn) (string, *llm.ChatResponse, error) {
	target := h.target()
	if target == nil {
		return "", nil, errors.New("markdown summarizer is unavailable")
	}
	return target.Summarize(ctx, turns)
}

// Get reads the cached markdown for one session generation from the current
// summarizer. A rotated summarizer starts with an empty cache; the next
// compaction recomputes from the transcript.
func (h *summarizerHolder) Get(userID, sessionID string, generation int) string {
	target := h.target()
	if target == nil {
		return ""
	}
	return target.Get(userID, sessionID, generation)
}

// Set stores cached markdown on the current summarizer.
func (h *summarizerHolder) Set(userID, sessionID string, generation int, markdown string) {
	if target := h.target(); target != nil {
		target.Set(userID, sessionID, generation, markdown)
	}
}

var _ summarizerTarget = (*compaction.Summarizer)(nil)
