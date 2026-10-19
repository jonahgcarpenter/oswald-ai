package compact

import (
	"context"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

// Store loads turns and resolves the active generation for manual compaction.
type Store interface {
	ResolveSessionContext(context.Context, string, string, time.Duration) (memory.SessionContext, error)
	RecentCompletedExchangesAfter(context.Context, string, string, int, int64, int) ([]memory.SessionTurn, error)
}

// Summarizer compacts turns to markdown and stores the ephemeral result.
type Summarizer interface {
	Summarize(context.Context, []memory.SessionTurn) (string, *llm.ChatResponse, error)
	Set(string, string, int, string)
}

type handler struct {
	store      Store
	summarizer Summarizer
	limit      int
}

// New creates the /compact command. Limit bounds the turn page; 0 uses 1000.
func New(store Store, summarizer Summarizer) commands.Handler {
	return &handler{store: store, summarizer: summarizer, limit: 1000}
}

func (h *handler) Definition() commands.Definition {
	return commands.Definition{Name: "compact", Summary: "Compact this session into markdown summary.", Usage: "/compact"}
}

func (h *handler) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	if len(req.Args) != 0 {
		return commands.Result{Text: commands.UsageText(h.Definition()), Outcome: commands.Outcome{Status: "rejected", ReasonCode: "invalid_arguments"}}, nil
	}
	if !req.Principal.Authenticated() {
		return commands.Result{Text: "Compacting requires an authenticated identity.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "authentication_required"}}, nil
	}
	if h.store == nil || h.summarizer == nil {
		return commands.Result{}, fmt.Errorf("compact is not configured")
	}
	session, err := h.store.ResolveSessionContext(ctx, req.Principal.CanonicalUserID, req.SessionKey, 24*time.Hour)
	if err != nil {
		return commands.Result{}, err
	}
	turns, err := h.store.RecentCompletedExchangesAfter(ctx, req.Principal.CanonicalUserID, req.SessionKey, session.Generation, 0, h.limit)
	if err != nil {
		return commands.Result{}, err
	}
	if len(turns) == 0 {
		return commands.Result{Text: "Nothing to compact yet.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "no_history"}}, nil
	}
	markdown, _, err := h.summarizer.Summarize(ctx, turns)
	if err != nil {
		return commands.Result{}, err
	}
	h.summarizer.Set(req.Principal.CanonicalUserID, req.SessionKey, session.Generation, markdown)
	return commands.Result{
		Text:    fmt.Sprintf("Compacted %d turns into a markdown summary. It applies to the next reply.", len(turns)),
		Outcome: commands.Outcome{Status: "ok", Operation: "session.compact", IsChanged: true, AffectedCount: len(turns)},
	}, nil
}
