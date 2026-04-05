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
// It is exported so the agent can compact retained turns when prompt budget
// pressure requires replacing older history with a shorter synthetic turn.
type Turn struct {
	CreatedAt time.Time
	User      ollama.ChatMessage
	Assistant ollama.ChatMessage
}

// session holds the accumulated conversation turns for a single session.
type session struct {
	turns []Turn
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

// History returns the retained user/assistant messages for the given session
// after destructively applying TTL and max-turn pruning.
func (s *Store) History(sessionKey string) []ollama.ChatMessage {
	return FlattenTurns(s.Turns(sessionKey))
}

// Turns returns the retained turn pairs for the given session after
// destructively applying TTL and max-turn pruning.
func (s *Store) Turns(sessionKey string) []Turn {
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

	result := PruneTurns(now, sess.turns, s.options)
	keptTurns := result.Kept
	removedExpired := result.RemovedExpired
	removedOverflow := result.RemovedOverflow
	if len(keptTurns) == 0 {
		delete(s.sessions, sessionKey)
		s.mu.Unlock()
		s.logPrune(sessionKey, removedExpired, removedOverflow, 0, "history")
		return nil
	}

	sess.turns = keptTurns
	s.mu.Unlock()

	s.logPrune(sessionKey, removedExpired, removedOverflow, len(keptTurns), "history")

	return keptTurns
}

// ReplaceTurns overwrites the retained turn pairs for a session after
// destructively applying TTL and max-turn pruning to the provided slice.
func (s *Store) ReplaceTurns(sessionKey string, turns []Turn) {
	if sessionKey == "" {
		return
	}

	now := time.Now().UTC()
	result := PruneTurns(now, turns, s.options)
	keptTurns := result.Kept
	removedExpired := result.RemovedExpired
	removedOverflow := result.RemovedOverflow

	s.mu.Lock()
	if len(keptTurns) == 0 {
		delete(s.sessions, sessionKey)
	} else {
		sess, ok := s.sessions[sessionKey]
		if !ok {
			sess = &session{}
			s.sessions[sessionKey] = sess
		}
		sess.turns = keptTurns
	}
	s.mu.Unlock()

	s.logPrune(sessionKey, removedExpired, removedOverflow, len(keptTurns), "replace")
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

	initial := PruneTurns(now, sess.turns, s.options)
	prunedTurns := initial.Kept
	removedExpired := initial.RemovedExpired
	removedOverflow := initial.RemovedOverflow
	prunedTurns = append(prunedTurns, Turn{
		CreatedAt: now,
		User:      user,
		Assistant: assistant,
	})
	postAppend := PruneTurns(now, prunedTurns, s.options)
	prunedTurns = postAppend.Kept
	postAppendExpired := postAppend.RemovedExpired
	postAppendOverflow := postAppend.RemovedOverflow
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

// FlattenTurns converts retained turn pairs into the interleaved user/assistant
// chat-history format expected by Ollama requests.
func FlattenTurns(turns []Turn) []ollama.ChatMessage {
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
