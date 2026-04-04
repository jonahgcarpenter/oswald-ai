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

// turn stores a single conversational exchange and when it entered memory.
type turn struct {
	CreatedAt time.Time
	User      ollama.ChatMessage
	Assistant ollama.ChatMessage
}

// session holds the accumulated conversation turns for a single session.
type session struct {
	turns      []turn
	lastPrompt promptEstimate
}

type promptEstimate struct {
	EstimatedBefore int
	EstimatedAfter  int
}

// Store is a concurrency-safe in-memory conversation history store.
// Each session is identified by a string key and retains bounded in-process
// user/assistant history until the process exits.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*session
	options  Options
	dumpPath string
	log      *config.Logger
}

type snapshot struct {
	GeneratedAt string                     `json:"generated_at"`
	Retention   snapshotRetention          `json:"retention"`
	Context     snapshotContext            `json:"context"`
	Sessions    map[string]snapshotSession `json:"sessions"`
}

type snapshotRetention struct {
	MaxTurns int    `json:"max_turns"`
	MaxAge   string `json:"max_age"`
}

type snapshotContext struct {
	ContextWindow int `json:"context_window"`
	PromptBudget  int `json:"prompt_budget"`
}

type snapshotSession struct {
	PromptEstimate snapshotPromptEstimate `json:"prompt_estimate"`
	Turns          []snapshotTurn         `json:"turns"`
}

type snapshotPromptEstimate struct {
	EstimatedBefore int `json:"estimated_before"`
	EstimatedAfter  int `json:"estimated_after"`
}

type snapshotTurn struct {
	CreatedAt string             `json:"created_at"`
	User      ollama.ChatMessage `json:"user"`
	Assistant ollama.ChatMessage `json:"assistant"`
}

// NewStore creates an in-memory conversation store.
func NewStore(options Options, dumpPath string, log *config.Logger) *Store {
	return &Store{
		sessions: make(map[string]*session),
		options:  sanitizeOptions(options),
		dumpPath: dumpPath,
		log:      log,
	}
}

// History returns a defensive copy of the stored messages for the given
// session key. Returns nil if the session does not exist or the key is empty.
// The returned slice contains only user and assistant messages - no system
// prompt - and is safe to prepend directly to a new chat message slice.
func (s *Store) History(sessionKey string) []ollama.ChatMessage {
	if sessionKey == "" {
		return nil
	}

	now := time.Now().UTC()

	s.mu.Lock()
	sess, ok := s.sessions[sessionKey]
	if !ok {
		s.mu.Unlock()
		return nil
	}

	prunedTurns, removedExpired, removedOverflow := s.pruneTurns(now, sess.turns)
	if len(prunedTurns) == 0 {
		delete(s.sessions, sessionKey)
		snap := s.snapshotLocked(now)
		s.mu.Unlock()
		s.logPrune(sessionKey, removedExpired, removedOverflow, 0, "history")
		s.dumpSnapshot(snap)
		return nil
	}

	sess.turns = prunedTurns
	history := flattenTurns(prunedTurns)
	snap := s.snapshotLocked(now)
	s.mu.Unlock()

	s.logPrune(sessionKey, removedExpired, removedOverflow, len(prunedTurns), "history")
	if removedExpired > 0 || removedOverflow > 0 {
		s.dumpSnapshot(snap)
	}

	return history
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

	prunedTurns, removedExpired, removedOverflow := s.pruneTurns(now, sess.turns)
	prunedTurns = append(prunedTurns, turn{
		CreatedAt: now,
		User:      user,
		Assistant: assistant,
	})
	prunedTurns, postAppendExpired, postAppendOverflow := s.pruneTurns(now, prunedTurns)
	removedExpired += postAppendExpired
	removedOverflow += postAppendOverflow

	if len(prunedTurns) == 0 {
		delete(s.sessions, sessionKey)
	} else {
		sess.turns = prunedTurns
	}

	turnCount := len(prunedTurns)
	s.log.Debug("Memory: session %q has %d turn(s) after append", sessionKey, turnCount)
	snap := s.snapshotLocked(now)
	s.mu.Unlock()

	s.logPrune(sessionKey, removedExpired, removedOverflow, turnCount, "append")
	s.dumpSnapshot(snap)
}

// RecordPromptEstimate stores the latest prompt token estimate for a session's
// retained history before and after context-budget pruning.
func (s *Store) RecordPromptEstimate(sessionKey string, estimatedBefore int, estimatedAfter int) {
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
	sess.lastPrompt = promptEstimate{
		EstimatedBefore: estimatedBefore,
		EstimatedAfter:  estimatedAfter,
	}
	snap := s.snapshotLocked(now)
	s.mu.Unlock()

	s.dumpSnapshot(snap)
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

func (s *Store) pruneTurns(now time.Time, turns []turn) ([]turn, int, int) {
	if len(turns) == 0 {
		return nil, 0, 0
	}

	kept := turns
	removedExpired := 0
	removedOverflow := 0

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
		kept = kept[firstValid:]
	}

	if s.options.MaxTurns > 0 && len(kept) > s.options.MaxTurns {
		removedOverflow = len(kept) - s.options.MaxTurns
		kept = kept[removedOverflow:]
	}

	if len(kept) == 0 {
		return nil, removedExpired, removedOverflow
	}

	cp := make([]turn, len(kept))
	copy(cp, kept)
	return cp, removedExpired, removedOverflow
}

func flattenTurns(turns []turn) []ollama.ChatMessage {
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

func (s *Store) snapshotLocked(now time.Time) snapshot {
	sessions := make(map[string]snapshotSession, len(s.sessions))
	for key, sess := range s.sessions {
		turns := make([]snapshotTurn, 0, len(sess.turns))
		for _, entry := range sess.turns {
			turns = append(turns, snapshotTurn{
				CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339),
				User:      entry.User,
				Assistant: entry.Assistant,
			})
		}
		sessions[key] = snapshotSession{
			PromptEstimate: snapshotPromptEstimate{
				EstimatedBefore: sess.lastPrompt.EstimatedBefore,
				EstimatedAfter:  sess.lastPrompt.EstimatedAfter,
			},
			Turns: turns,
		}
	}

	return snapshot{
		GeneratedAt: now.Format(time.RFC3339),
		Retention: snapshotRetention{
			MaxTurns: s.options.MaxTurns,
			MaxAge:   s.options.MaxAge.String(),
		},
		Context: snapshotContext{
			ContextWindow: s.options.ContextWindow,
			PromptBudget:  s.options.PromptBudget,
		},
		Sessions: sessions,
	}
}

func (s *Store) dumpSnapshot(snap snapshot) {
	if s.dumpPath == "" {
		return
	}

	if err := WriteSection(s.dumpPath, "memory", snap); err != nil {
		s.log.Warn("Memory: failed to write debug snapshot to %q: %v", s.dumpPath, err)
		return
	}

	s.log.Debug("Memory: wrote debug snapshot section to %q", s.dumpPath)
}
