package soulmemory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	return s.writeLocked(content)
}

func (s *Store) writeLocked(content string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, []byte(content), 0o644); err != nil {
		return err
	}
	s.log.Debug("memory.soul.written", "wrote soul file", config.F("path", s.path), config.F("content_chars", len(content)))
	return nil
}

// Patch applies a single-line mutation to the soul file.
func (s *Store) Patch(operation, target, content, position, anchor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.readLocked()
	if err != nil {
		return err
	}

	updated, err := patchContent(current, operation, target, content, position, anchor)
	if err != nil {
		return err
	}

	return s.writeLocked(updated)
}

func (s *Store) readLocked() (string, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func patchContent(current, operation, target, content, position, anchor string) (string, error) {
	lines, hasTrailingNewline := splitLinesPreserveTrailingNewline(current)

	switch operation {
	case "replace":
		index, err := findUniqueLine(lines, target, "target")
		if err != nil {
			return "", err
		}
		lines[index] = content
	case "remove":
		index, err := findUniqueLine(lines, target, "target")
		if err != nil {
			return "", err
		}
		lines = append(lines[:index], lines[index+1:]...)
	case "add":
		switch position {
		case "end":
			lines = append(lines, content)
		case "before", "after":
			index, err := findUniqueLine(lines, anchor, "anchor")
			if err != nil {
				return "", err
			}
			insertAt := index
			if position == "after" {
				insertAt = index + 1
			}
			lines = append(lines[:insertAt], append([]string{content}, lines[insertAt:]...)...)
		default:
			return "", fmt.Errorf("invalid add position %q", position)
		}
	default:
		return "", fmt.Errorf("invalid patch operation %q", operation)
	}

	return joinLinesPreserveTrailingNewline(lines, hasTrailingNewline), nil
}

func splitLinesPreserveTrailingNewline(content string) ([]string, bool) {
	if content == "" {
		return nil, false
	}
	hasTrailingNewline := strings.HasSuffix(content, "\n")
	trimmed := strings.TrimSuffix(content, "\n")
	return strings.Split(trimmed, "\n"), hasTrailingNewline
}

func joinLinesPreserveTrailingNewline(lines []string, hadTrailingNewline bool) string {
	if len(lines) == 0 {
		return ""
	}
	joined := strings.Join(lines, "\n")
	if hadTrailingNewline {
		return joined + "\n"
	}
	return joined
}

func findUniqueLine(lines []string, needle, label string) (int, error) {
	matchIndex := -1
	for i, line := range lines {
		if line != needle {
			continue
		}
		if matchIndex >= 0 {
			return -1, fmt.Errorf("%s matched multiple lines", label)
		}
		matchIndex = i
	}
	if matchIndex < 0 {
		return -1, fmt.Errorf("%s line not found", label)
	}
	return matchIndex, nil
}
