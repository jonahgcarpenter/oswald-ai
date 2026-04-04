package soulmemory

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Store manages the soul file — a single Markdown document that serves as the
// agent's system prompt and identity definition. All reads and writes are
// protected by a RWMutex to allow concurrent reads while serializing writes.
type Store struct {
	path string
	log  *config.Logger
	mu   sync.RWMutex
}

// NewStore creates a Store that manages the soul file at the given path.
func NewStore(path string, log *config.Logger) *Store {
	return &Store{
		path: path,
		log:  log,
	}
}

// Read returns the full contents of the soul file. Returns an empty string
// (not an error) if the file does not yet exist.
func (s *Store) Read() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Write replaces the entire soul file with the provided content, creating the
// file and any necessary parent directories if they do not exist.
func (s *Store) Write(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, []byte(content), 0o644); err != nil {
		return err
	}
	s.log.Debug("Soul file written (%d bytes): %s", len(content), s.path)
	return nil
}

// Append adds content to the end of the soul file. If the file does not exist
// it is created with the provided content.
func (s *Store) Append(content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(content)
	if err != nil {
		return err
	}
	s.log.Debug("Soul file appended (%d bytes): %s", len(content), s.path)
	return nil
}
