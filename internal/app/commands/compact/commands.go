package compact

import (
	"context"
	"fmt"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/agent"
	"github.com/jonahgcarpenter/oswald-ai/internal/app/commands"
	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/compaction"
	tokenbudget "github.com/jonahgcarpenter/oswald-ai/internal/session/compaction/budget"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

// Store loads turns and resolves the active generation for manual compaction.
type Store interface {
	ResolveSessionContext(context.Context, string, string, time.Duration) (memory.SessionContext, error)
	RecentCompletedExchangesAfter(context.Context, string, string, int, int64, int) ([]memory.SessionTurn, error)
	MarkSessionTurnsCompacted(context.Context, string, string, int, []int64) error
}

// Summarizer compacts turns to markdown and stores the ephemeral result.
type Summarizer interface {
	Summarize(context.Context, []memory.SessionTurn) (string, *llm.ChatResponse, error)
	Set(string, string, int, string)
}

// Router resolves the session's selected model so manual compaction runs on
// the same model as the conversation (issue #295). Nil disables resolution and
// falls back to the summarizer's pinned model.
type Router interface {
	Resolve(ctx context.Context, owner, sessionKey, gateway, chatID string) (agent.Target, []agent.LinkDegradation)
}

type handler struct {
	store      Store
	summarizer Summarizer
	router     Router
	limit      int
}

// New creates the /compact command. Limit bounds the turn page; 0 uses 1000.
// An optional router resolves the session model for target-aware compaction.
func New(store Store, summarizer Summarizer, router ...Router) commands.Handler {
	h := &handler{store: store, summarizer: summarizer, limit: 1000}
	if len(router) > 0 {
		h.router = router[0]
	}
	return h
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
	candidates := tokenbudget.Candidates(turns)
	if len(candidates) == 0 {
		return commands.Result{Text: "Nothing to compact yet.", Outcome: commands.Outcome{Status: "rejected", ReasonCode: "no_history"}}, nil
	}
	markdown, err := h.summarizeWithSessionModel(ctx, req, candidates)
	if err != nil {
		return commands.Result{}, err
	}
	h.summarizer.Set(req.Principal.CanonicalUserID, req.SessionKey, session.Generation, markdown)
	if err := h.store.MarkSessionTurnsCompacted(ctx, req.Principal.CanonicalUserID, req.SessionKey, session.Generation, compactedTurnIDs(candidates)); err != nil {
		return commands.Result{}, err
	}
	return commands.Result{
		Text:    fmt.Sprintf("Compacted %d turns into a markdown summary. It applies to the next reply.", len(candidates)),
		Outcome: commands.Outcome{Status: "ok", Operation: "session.compact", IsChanged: true, AffectedCount: len(candidates)},
	}, nil
}

// compactedTurnIDs returns the assistant turn IDs for compaction bookkeeping.
func compactedTurnIDs(turns []memory.SessionTurn) []int64 {
	ids := make([]int64, 0, len(turns))
	for _, turn := range turns {
		if turn.ID > 0 {
			ids = append(ids, turn.ID)
		}
	}
	return ids
}

// summarizeWithSessionModel runs compaction on the session's resolved model so
// manual /compact matches the conversation (issue #295). Without a router it
// falls back to the summarizer's pinned model.
func (h *handler) summarizeWithSessionModel(ctx context.Context, req commands.Request, candidates []memory.SessionTurn) (string, error) {
	if h.router != nil {
		target, _ := h.router.Resolve(ctx, req.Principal.CanonicalUserID, req.SessionKey, req.Principal.Gateway, req.ChatID)
		if target.Client != nil && target.Model != "" {
			if markdown, _, err := compaction.SummarizeWithClient(ctx, target.Client, target.Model, candidates); err == nil {
				return markdown, nil
			}
		}
	}
	markdown, _, err := h.summarizer.Summarize(ctx, candidates)
	return markdown, err
}
