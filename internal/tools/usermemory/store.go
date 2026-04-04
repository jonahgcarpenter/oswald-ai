package usermemory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Store manages persistent per-user Markdown memory files.
// Each user gets a single file at <basedir>/<userID>.md that the model
// reads and writes via the persistent_memory tool. Files survive restarts.
//
// Concurrent access to the same user file is serialised with a per-user mutex.
// Access to different user files is fully parallel.
type Store struct {
	basedir string
	log     *config.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewStore creates a Store that persists user memory files under basedir.
// The directory is created on first use rather than at startup, so a
// zero-value path simply disables persistence until it is set.
func NewStore(basedir string, log *config.Logger) *Store {
	return &Store{
		basedir: basedir,
		log:     log,
		locks:   make(map[string]*sync.Mutex),
	}
}

// lockFor returns (and lazily creates) the per-user mutex for userID.
func (s *Store) lockFor(userID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.locks[userID]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.locks[userID] = m
	return m
}

// filePath returns the absolute path for a user's memory file.
func (s *Store) filePath(userID string) string {
	return filepath.Join(s.basedir, userID+".md")
}

// Read returns the full contents of the user's memory file.
// Returns an empty string if the file does not exist yet (not an error).
func (s *Store) Read(userID string) (string, error) {
	l := s.lockFor(userID)
	l.Lock()
	defer l.Unlock()

	data, err := os.ReadFile(s.filePath(userID))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read user memory for %q: %w", userID, err)
	}
	return string(data), nil
}

// Set stores or updates a single key/value fact in the user's memory file.
// The file is structured as a Markdown list. If a line for the key already
// exists it is replaced in place; otherwise a new line is appended.
func (s *Store) Set(userID, key, value string) error {
	l := s.lockFor(userID)
	l.Lock()
	defer l.Unlock()

	path := s.filePath(userID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create memory directory: %w", err)
	}

	var existing string
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read user memory for %q: %w", userID, err)
	}
	if err == nil {
		existing = string(data)
	}

	updated, replaced := replaceFact(existing, key, value)
	if !replaced {
		if existing == "" {
			updated = fmt.Sprintf("# User Memory\n\n- **%s**: %s\n", key, value)
		} else {
			updated = strings.TrimRight(existing, "\n") + fmt.Sprintf("\n- **%s**: %s\n", key, value)
		}
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}

	s.log.Debug("UserMemory: set key=%q for user=%q", key, userID)
	return nil
}

// Delete removes a single fact by key from the user's memory file.
// Returns nil (not an error) if the file or key does not exist.
func (s *Store) Delete(userID, key string) error {
	l := s.lockFor(userID)
	l.Lock()
	defer l.Unlock()

	path := s.filePath(userID)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read user memory for %q: %w", userID, err)
	}

	updated := deleteFact(string(data), key)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}

	s.log.Debug("UserMemory: deleted key=%q for user=%q", key, userID)
	return nil
}

// DeleteAll removes the user's entire memory file.
// Returns nil if the file does not exist.
func (s *Store) DeleteAll(userID string) error {
	l := s.lockFor(userID)
	l.Lock()
	defer l.Unlock()

	err := os.Remove(s.filePath(userID))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to delete user memory for %q: %w", userID, err)
	}

	s.log.Debug("UserMemory: wiped all memory for user=%q", userID)
	return nil
}

// factPattern matches a Markdown list line of the form: - **key**: value
// The key is captured in group 1.
var factPattern = regexp.MustCompile(`(?m)^- \*\*([^*]+)\*\*:.*$`)

// replaceFact replaces the line for key with a new value in content.
// Returns the updated content and true if a replacement was made.
func replaceFact(content, key, value string) (string, bool) {
	replaced := false
	updated := factPattern.ReplaceAllStringFunc(content, func(line string) string {
		matches := factPattern.FindStringSubmatch(line)
		if len(matches) >= 2 && strings.EqualFold(matches[1], key) {
			replaced = true
			return fmt.Sprintf("- **%s**: %s", key, value)
		}
		return line
	})
	return updated, replaced
}

// deleteFact removes the line for key from content and returns the result.
func deleteFact(content, key string) string {
	lines := strings.Split(content, "\n")
	kept := lines[:0]
	for _, line := range lines {
		matches := factPattern.FindStringSubmatch(line)
		if len(matches) >= 2 && strings.EqualFold(matches[1], key) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
