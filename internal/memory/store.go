package memory

import (
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/debugdump"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// session holds the accumulated conversation turns for a single session and
// the last time it was updated, used for idle-session expiry.
type session struct {
	messages  []ollama.ChatMessage
	updatedAt time.Time
}

// Store is a concurrency-safe in-memory conversation history store.
// Each session is identified by a string key and holds a sliding window of
// the most recent user/assistant turn pairs. Sessions that have been idle
// longer than maxAge are evicted automatically by the background reaper.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*session
	maxTurns int
	maxAge   time.Duration
	dumpPath string
	log      *config.Logger
}

type snapshot struct {
	GeneratedAt string                     `json:"generated_at"`
	Sessions    map[string]snapshotSession `json:"sessions"`
}

type snapshotSession struct {
	UpdatedAt string               `json:"updated_at"`
	Messages  []ollama.ChatMessage `json:"messages"`
}

// NewStore creates a Store and starts the background reaper goroutine.
// maxTurns is the maximum number of user+assistant turn pairs to retain per
// session (each pair is 2 messages). maxAge is the idle TTL after which a
// session is evicted.
func NewStore(maxTurns int, maxAge time.Duration, dumpPath string, log *config.Logger) *Store {
	s := &Store{
		sessions: make(map[string]*session),
		maxTurns: maxTurns,
		maxAge:   maxAge,
		dumpPath: dumpPath,
		log:      log,
	}
	go s.startReaper()
	return s
}

// History returns a defensive copy of the stored messages for the given
// session key. Returns nil if the session does not exist or the key is empty.
// The returned slice contains only user and assistant messages — no system
// prompt — and is safe to prepend directly to a new chat message slice.
func (s *Store) History(sessionKey string) []ollama.ChatMessage {
	if sessionKey == "" {
		return nil
	}

	s.mu.RLock()
	sess, ok := s.sessions[sessionKey]
	s.mu.RUnlock()

	if !ok || len(sess.messages) == 0 {
		return nil
	}

	cp := make([]ollama.ChatMessage, len(sess.messages))
	copy(cp, sess.messages)
	return cp
}

// Append adds one or more messages to the session identified by sessionKey,
// then enforces the sliding-window limit by evicting the oldest turn pairs
// from the front until the message count is within maxTurns*2.
// If sessionKey is empty, Append is a no-op.
func (s *Store) Append(sessionKey string, msgs ...ollama.ChatMessage) {
	if sessionKey == "" || len(msgs) == 0 {
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[sessionKey]
	if !ok {
		sess = &session{}
		s.sessions[sessionKey] = sess
	}

	sess.messages = append(sess.messages, msgs...)
	sess.updatedAt = time.Now()

	// Enforce the sliding window: each "turn" is one user message + one
	// assistant message. Evict pairs from the front until we are within
	// the limit. We always evict in pairs so the history stays coherent
	// (never starts mid-turn).
	limit := s.maxTurns * 2
	for len(sess.messages) > limit {
		// Drop two messages (one user + one assistant turn pair) from the front.
		if len(sess.messages) >= 2 {
			sess.messages = sess.messages[2:]
		} else {
			sess.messages = sess.messages[:0]
		}
	}

	s.log.Debug("Memory: session %q has %d message(s) after append", sessionKey, len(sess.messages))
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.dumpSnapshot(snap)
}

// startReaper runs as a background goroutine, pruning idle sessions every
// 5 minutes. Sessions that have not been updated within maxAge are removed.
func (s *Store) startReaper() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.prune()
	}
}

// prune removes all sessions whose updatedAt is older than maxAge.
func (s *Store) prune() {
	cutoff := time.Now().Add(-s.maxAge)

	s.mu.Lock()
	pruned := 0
	for key, sess := range s.sessions {
		if sess.updatedAt.Before(cutoff) {
			delete(s.sessions, key)
			pruned++
		}
	}

	if pruned > 0 {
		s.log.Debug("Memory: pruned %d idle session(s) (maxAge=%s)", pruned, s.maxAge)
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.dumpSnapshot(snap)
}

func (s *Store) snapshotLocked() snapshot {
	sessions := make(map[string]snapshotSession, len(s.sessions))
	for key, sess := range s.sessions {
		messages := make([]ollama.ChatMessage, len(sess.messages))
		copy(messages, sess.messages)
		sessions[key] = snapshotSession{
			UpdatedAt: sess.updatedAt.UTC().Format(time.RFC3339),
			Messages:  messages,
		}
	}

	return snapshot{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Sessions:    sessions,
	}
}

func (s *Store) dumpSnapshot(snap snapshot) {
	if s.dumpPath == "" {
		return
	}

	if err := debugdump.WriteSection(s.dumpPath, "memory", snap); err != nil {
		s.log.Warn("Memory: failed to write debug snapshot to %q: %v", s.dumpPath, err)
		return
	}

	s.log.Debug("Memory: wrote debug snapshot section to %q", s.dumpPath)
}
