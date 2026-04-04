package usermemory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// Store manages persistent per-user Markdown memory files.
// Each user gets a single file at <basedir>/<userID>.md containing a list of
// remembered facts. Each fact is stored as a statement sentence followed by an
// evidence bullet on the next line, separated from other entries by a blank line:
//
//	The user's age is 23.
//
//	- Evidence: User stated "If I'm 23 how much will I have at 65". Date: [2025-12-10].
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
// The directory is created on first use rather than at startup.
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

// Set stores a new fact or replaces an existing one whose statement matches.
// Each entry is written as:
//
//	<statement>
//
//	- Evidence: <evidence>. Date: [<today>].
//
// If an entry with an identical statement (case-insensitive) already exists it
// is replaced in place; otherwise the new entry is appended.
func (s *Store) Set(userID, statement, evidence string) error {
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

	entry := formatEntry(statement, evidence)
	updated, replaced := replaceEntry(existing, statement, entry)
	if !replaced {
		if existing == "" {
			updated = "# User Memory\n\n" + entry
		} else {
			updated = strings.TrimRight(existing, "\n") + "\n\n" + entry
		}
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}

	s.log.Debug("UserMemory: remembered statement for user=%q", userID)
	return nil
}

// Delete removes the entry whose statement matches the given text.
// Returns nil if the file or entry does not exist.
func (s *Store) Delete(userID, statement string) error {
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

	updated := deleteEntry(string(data), statement)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}

	s.log.Debug("UserMemory: deleted entry for user=%q", userID)
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

// formatEntry builds the two-line Markdown block for a single fact.
func formatEntry(statement, evidence string) string {
	date := time.Now().Format("2006-01-02")
	return fmt.Sprintf("%s\n\n- Evidence: %s. Date: [%s].\n", statement, evidence, date)
}

// parseEntries splits a memory file into individual entry blocks.
// Each entry is the text between blank-line separators (excluding the header).
// Returns a slice where each element is one complete entry block (trimmed).
func parseEntries(content string) []string {
	// Strip the # User Memory header paragraph if present
	body := content
	if strings.HasPrefix(content, "# User Memory") {
		idx := strings.Index(content, "\n\n")
		if idx >= 0 {
			body = content[idx+2:]
		}
	}

	// Split on double newlines to get candidate blocks
	raw := strings.Split(body, "\n\n")
	var entries []string
	for _, block := range raw {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		// A valid entry must have at least two lines: the statement and the evidence line
		lines := strings.SplitN(block, "\n", 2)
		if len(lines) == 2 && strings.HasPrefix(strings.TrimSpace(lines[1]), "- Evidence:") {
			entries = append(entries, block)
		}
	}
	return entries
}

// statementOf extracts the statement line (first line) from an entry block.
func statementOf(entry string) string {
	lines := strings.SplitN(entry, "\n", 2)
	return strings.TrimSpace(lines[0])
}

// replaceEntry replaces the entry whose statement matches the given text in place.
// Returns the updated content and whether a replacement occurred.
func replaceEntry(content, statement, newEntry string) (string, bool) {
	entries := parseEntries(content)
	replaced := false
	for i, e := range entries {
		if strings.EqualFold(statementOf(e), strings.TrimSpace(statement)) {
			entries[i] = strings.TrimSpace(newEntry)
			replaced = true
			break
		}
	}
	if !replaced {
		return content, false
	}
	return "# User Memory\n\n" + strings.Join(entries, "\n\n") + "\n", true
}

// deleteEntry removes the entry whose statement matches from content and returns the result.
func deleteEntry(content, statement string) string {
	entries := parseEntries(content)
	kept := entries[:0]
	for _, e := range entries {
		if !strings.EqualFold(statementOf(e), strings.TrimSpace(statement)) {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return "# User Memory\n"
	}
	return "# User Memory\n\n" + strings.Join(kept, "\n\n") + "\n"
}
