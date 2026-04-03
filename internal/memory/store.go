package memory

import (
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/debugdump"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
)

// session holds the accumulated conversation turns for a single session.
type session struct {
	messages []ollama.ChatMessage
}

// Store is a concurrency-safe in-memory conversation history store.
// Each session is identified by a string key and retains its full in-process
// user/assistant history until the process exits.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*session
	dumpPath string
	log      *config.Logger
}

type snapshot struct {
	GeneratedAt string                     `json:"generated_at"`
	Sessions    map[string]snapshotSession `json:"sessions"`
}

type snapshotSession struct {
	Messages []ollama.ChatMessage `json:"messages"`
}

// NewStore creates an in-memory conversation store.
func NewStore(dumpPath string, log *config.Logger) *Store {
	s := &Store{
		sessions: make(map[string]*session),
		dumpPath: dumpPath,
		log:      log,
	}
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

// Append adds one or more messages to the session identified by sessionKey.
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

	s.log.Debug("Memory: session %q has %d message(s) after append", sessionKey, len(sess.messages))
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
			Messages: messages,
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
