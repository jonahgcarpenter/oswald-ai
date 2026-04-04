package memory

import (
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// Options controls in-process conversation retention.
type Options struct {
	MaxTurns      int
	MaxAge        time.Duration
	ContextWindow int
	PromptBudget  int
}

// Turn stores a single conversational exchange and when it entered memory.
// It is exported so the agent can pass pruned turns to the summarizer.
type Turn struct {
	CreatedAt time.Time
	User      ollama.ChatMessage
	Assistant ollama.ChatMessage
}

// session holds the accumulated conversation turns for a single session.
type session struct {
	turns   []Turn
	summary string // rolling summary of turns pruned from this session
}

// Store is a concurrency-safe in-memory conversation history store.
// Each session is identified by a string key and retains bounded in-process
// user/assistant history until the process exits.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*session
	options  Options
	log      *config.Logger
}

// NewStore creates an in-memory conversation store.
func NewStore(options Options, log *config.Logger) *Store {
	return &Store{
		sessions: make(map[string]*session),
		options:  sanitizeOptions(options),
		log:      log,
	}
}

// History returns a defensive copy of the stored messages for the given
// session key. Returns nil if the session does not exist or the key is empty.
// The returned slice contains only user and assistant messages - no system
// prompt - and is safe to prepend directly to a new chat message slice.
func (s *Store) History(sessionKey string) []ollama.ChatMessage {
	msgs, _, _ := s.HistoryWithPruneInfo(sessionKey)
	return msgs
}

// HistoryWithPruneInfo returns the flattened history messages alongside any
// turns that were permanently removed by retention pruning and the current
// rolling summary for this session. The caller can use the pruned turns to
// generate a summary before they are lost.
func (s *Store) HistoryWithPruneInfo(sessionKey string) (history []ollama.ChatMessage, prunedTurns []Turn, existingSummary string) {
	if sessionKey == "" {
		return nil, nil, ""
	}

	now := time.Now().UTC()

	s.mu.Lock()
	sess, ok := s.sessions[sessionKey]
	if !ok {
		s.mu.Unlock()
		return nil, nil, ""
	}

	existingSummary = sess.summary
	keptTurns, removedTurns, removedExpired, removedOverflow := s.pruneTurns(now, sess.turns)
	if len(keptTurns) == 0 {
		delete(s.sessions, sessionKey)
		s.mu.Unlock()
		s.logPrune(sessionKey, removedExpired, removedOverflow, 0, "history")
		return nil, removedTurns, existingSummary
	}

	sess.turns = keptTurns
	history = flattenTurns(keptTurns)
	s.mu.Unlock()

	s.logPrune(sessionKey, removedExpired, removedOverflow, len(keptTurns), "history")

	return history, removedTurns, existingSummary
}

// SetSummary stores a rolling summary of pruned turns for the given session.
// The summary is injected into the system prompt on subsequent requests so the
// model retains context from conversations older than the active window.
func (s *Store) SetSummary(sessionKey string, summary string) {
	if sessionKey == "" {
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[sessionKey]
	if !ok {
		sess = &session{}
		s.sessions[sessionKey] = sess
	}
	sess.summary = summary
	s.mu.Unlock()
}

// AppendTurn adds a user/assistant turn pair to the session identified by sessionKey.
// If sessionKey is empty, AppendTurn is a no-op.
func (s *Store) AppendTurn(sessionKey string, user ollama.ChatMessage, assistant ollama.ChatMessage) {
	if sessionKey == "" {
		return
	}

	now := time.Now().UTC()

	s.mu.Lock()
	sess, ok := s.sessions[sessionKey]
	if !ok {
		sess = &session{}
		s.sessions[sessionKey] = sess
	}

	prunedTurns, _, removedExpired, removedOverflow := s.pruneTurns(now, sess.turns)
	prunedTurns = append(prunedTurns, Turn{
		CreatedAt: now,
		User:      user,
		Assistant: assistant,
	})
	prunedTurns, _, postAppendExpired, postAppendOverflow := s.pruneTurns(now, prunedTurns)
	removedExpired += postAppendExpired
	removedOverflow += postAppendOverflow

	if len(prunedTurns) == 0 {
		delete(s.sessions, sessionKey)
	} else {
		sess.turns = prunedTurns
	}

	turnCount := len(prunedTurns)
	s.log.Debug("Memory: session %q has %d turn(s) after append", sessionKey, turnCount)
	s.mu.Unlock()

	s.logPrune(sessionKey, removedExpired, removedOverflow, turnCount, "append")
}

func sanitizeOptions(options Options) Options {
	if options.MaxTurns < 0 {
		options.MaxTurns = 0
	}
	if options.MaxAge < 0 {
		options.MaxAge = 0
	}
	if options.ContextWindow < 0 {
		options.ContextWindow = 0
	}
	if options.PromptBudget < 0 {
		options.PromptBudget = 0
	}
	return options
}

func (s *Store) pruneTurns(now time.Time, turns []Turn) (kept []Turn, removed []Turn, removedExpired int, removedOverflow int) {
	if len(turns) == 0 {
		return nil, nil, 0, 0
	}

	kept = turns
	removedExpired = 0
	removedOverflow = 0

	if s.options.MaxAge > 0 {
		cutoff := now.Add(-s.options.MaxAge)
		firstValid := len(kept)
		for i, candidate := range kept {
			if !candidate.CreatedAt.Before(cutoff) {
				firstValid = i
				break
			}
		}
		removedExpired = firstValid
		removed = append(removed, kept[:firstValid]...)
		kept = kept[firstValid:]
	}

	if s.options.MaxTurns > 0 && len(kept) > s.options.MaxTurns {
		removedOverflow = len(kept) - s.options.MaxTurns
		removed = append(removed, kept[:removedOverflow]...)
		kept = kept[removedOverflow:]
	}

	if len(kept) == 0 {
		return nil, removed, removedExpired, removedOverflow
	}

	cp := make([]Turn, len(kept))
	copy(cp, kept)
	return cp, removed, removedExpired, removedOverflow
}

func flattenTurns(turns []Turn) []ollama.ChatMessage {
	if len(turns) == 0 {
		return nil
	}

	history := make([]ollama.ChatMessage, 0, len(turns)*2)
	for _, entry := range turns {
		history = append(history, entry.User, entry.Assistant)
	}
	return history
}

func (s *Store) logPrune(sessionKey string, removedExpired int, removedOverflow int, retained int, operation string) {
	if removedExpired == 0 && removedOverflow == 0 {
		return
	}

	s.log.Debug(
		"Memory: pruned session %q during %s (expired=%d overflow=%d retained_turns=%d max_turns=%d max_age=%s)",
		sessionKey,
		operation,
		removedExpired,
		removedOverflow,
		retained,
		s.options.MaxTurns,
		s.options.MaxAge,
	)
}
