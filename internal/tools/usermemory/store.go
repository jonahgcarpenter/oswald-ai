package usermemory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// ValidCategories lists the supported memory categories in display order.
// The model can assign any of these when storing a fact.
var ValidCategories = []string{"identity", "system_rules", "preferences", "notes"}

// defaultCategory is used when the model omits the category argument.
const defaultCategory = "notes"

// Store manages persistent per-user Markdown memory files.
// Each user gets a single file at <basedir>/<userID>.md.
// Facts are organised into category sections (## Identity, ## Preferences,
// ## Context, ## Notes). Within each section, facts use the same two-line
// statement + evidence format:
//
//	## Identity
//
//	The user's name is Alex.
//
//	- Evidence: User stated "my name is Alex". Date: [2026-04-04].
//
// Files written in the old flat format (no ## category headers) are
// automatically migrated to the ## Notes section on first write.
//
// Concurrent access to the same user file is serialised with a per-user mutex.
// Access to different user files is fully parallel.
type Store struct {
	basedir string
	log     *config.Logger

	speakerLineResolver func(string) (string, error)

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

// SetSpeakerLineResolver configures how speaker intro lines are derived.
func (s *Store) SetSpeakerLineResolver(resolver func(string) (string, error)) {
	s.speakerLineResolver = resolver
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

// ReadIntro returns the top intro block from the user's memory file.
func (s *Store) ReadIntro(userID string) (string, error) {
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

	intro := strings.TrimSpace(parseMemory(string(data)).Intro)
	if !strings.HasPrefix(intro, "You are speaking with ") {
		return "", nil
	}
	return intro, nil
}

// SyncSpeakerIntro creates or updates the user's memory file so its intro block
// mirrors the supplied speaker line while preserving any existing sections.
func (s *Store) SyncSpeakerIntro(userID, intro string) error {
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
		existing = migrateIfNeeded(string(data))
	}

	parsed := parseMemory(existing)
	updated := serializeMemory(strings.TrimSpace(intro), parsed.Sections)
	if strings.TrimSpace(updated) == strings.TrimSpace(existing) {
		return nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}

	s.log.Debug("UserMemory: synced speaker intro for user=%q", userID)
	return nil
}

// Set stores a new fact or replaces an existing one whose statement matches,
// within the given category section. If category is empty, defaultCategory is used.
// Each entry is written as:
//
//	<statement>
//
//	- Evidence: <evidence>. Date: [<today>].
//
// If an entry with an identical statement (case-insensitive) already exists
// anywhere in the file it is replaced in place; otherwise the new entry is
// appended to the appropriate category section.
func (s *Store) Set(userID, statement, evidence, category string) error {
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
		existing = migrateIfNeeded(string(data))
	}

	cat := normalizeCategory(category)
	entry := formatEntry(statement, evidence)
	updated := replaceOrAppendCategorized(existing, statement, entry, cat)
	updated, err = s.withSpeakerIntro(userID, updated)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}

	s.log.Debug("UserMemory: remembered statement for user=%q category=%q", userID, cat)
	return nil
}

// ReadCategory returns only the facts stored under the given category section.
// Returns an empty string if the section does not exist or the file is missing.
func (s *Store) ReadCategory(userID, category string) (string, error) {
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

	cat := normalizeCategory(category)
	sections := parseSections(migrateIfNeeded(string(data)))
	content, ok := sections[cat]
	if !ok || strings.TrimSpace(content) == "" {
		return "", nil
	}
	return "## " + displayCategoryName(cat) + "\n\n" + strings.TrimSpace(content) + "\n", nil
}

// Delete removes the entry whose statement matches the given text.
// The search spans all category sections. Returns nil if the file or entry does not exist.
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

	updated := deleteCategorizedEntry(migrateIfNeeded(string(data)), statement)
	updated, err = s.withSpeakerIntro(userID, updated)
	if err != nil {
		return err
	}
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

// WriteFull atomically replaces the entire content of the user's memory file.
// It is used by the LLM migration path to persist a freshly categorized file.
func (s *Store) WriteFull(userID, content string) error {
	l := s.lockFor(userID)
	l.Lock()
	defer l.Unlock()

	path := s.filePath(userID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create memory directory: %w", err)
	}
	var err error
	content, err = s.withSpeakerIntro(userID, content)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}
	s.log.Debug("UserMemory: wrote full memory file for user=%q (%d bytes)", userID, len(content))
	return nil
}

// MergeUsers merges the persistent memory file for loserUserID into winnerUserID.
// Statement lines are de-duplicated case-insensitively, preserving the winner's
// existing entry when a duplicate exists in both files.
func (s *Store) MergeUsers(winnerUserID, loserUserID string) error {
	if winnerUserID == "" || loserUserID == "" || winnerUserID == loserUserID {
		return nil
	}

	firstID, secondID := winnerUserID, loserUserID
	if firstID > secondID {
		firstID, secondID = secondID, firstID
	}

	firstLock := s.lockFor(firstID)
	secondLock := s.lockFor(secondID)
	firstLock.Lock()
	secondLock.Lock()
	defer secondLock.Unlock()
	defer firstLock.Unlock()

	winnerPath := s.filePath(winnerUserID)
	loserPath := s.filePath(loserUserID)

	winnerRaw, err := os.ReadFile(winnerPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read user memory for %q: %w", winnerUserID, err)
	}
	loserRaw, err := os.ReadFile(loserPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read user memory for %q: %w", loserUserID, err)
	}

	merged := mergeCategorizedContent(string(winnerRaw), string(loserRaw))
	if strings.TrimSpace(merged) != "" && strings.TrimSpace(merged) != "# User Memory" {
		if err := os.MkdirAll(filepath.Dir(winnerPath), 0o755); err != nil {
			return fmt.Errorf("failed to create memory directory: %w", err)
		}
		if err := os.WriteFile(winnerPath, []byte(merged), 0o644); err != nil {
			return fmt.Errorf("failed to write merged user memory for %q: %w", winnerUserID, err)
		}
	}

	if err := os.Remove(loserPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove merged user memory for %q: %w", loserUserID, err)
	}

	s.log.Debug("UserMemory: merged user memory from %q into %q", loserUserID, winnerUserID)
	return nil
}

func (s *Store) withSpeakerIntro(userID, content string) (string, error) {
	parsed := parseMemory(content)
	if s.speakerLineResolver == nil {
		return serializeMemory(parsed.Intro, parsed.Sections), nil
	}

	intro, err := s.speakerLineResolver(userID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve speaker line for %q: %w", userID, err)
	}
	if strings.TrimSpace(intro) == "" {
		intro = parsed.Intro
	}
	return serializeMemory(intro, parsed.Sections), nil
}

// normalizeCategory maps an input category string to a valid lowercase category name.
// Falls back to defaultCategory if the input does not match any known category.
func normalizeCategory(cat string) string {
	cat = strings.TrimSpace(strings.ToLower(cat))
	cat = strings.ReplaceAll(cat, " ", "_")
	for _, valid := range ValidCategories {
		if cat == valid {
			return cat
		}
	}
	return defaultCategory
}

// displayCategoryName returns the human-readable Markdown heading for a category.
func displayCategoryName(cat string) string {
	parts := strings.Split(normalizeCategory(cat), "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		r := []rune(part)
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		parts[i] = string(r)
	}
	return strings.Join(parts, " ")
}

// formatEntry builds the two-line Markdown block for a single fact.
func formatEntry(statement, evidence string) string {
	evidence = sanitizeEvidence(evidence)
	date := time.Now().Format("2006-01-02")
	if hasExplicitEvidenceDate(evidence) {
		return fmt.Sprintf("%s\n\n- Evidence: %s.\n", statement, evidence)
	}
	return fmt.Sprintf("%s\n\n- Evidence: %s. Date: [%s].\n", statement, evidence, date)
}

var explicitEvidenceDateRE = regexp.MustCompile(`(?i)\bDate:\s*\[[^\]]+\]$`)

func sanitizeEvidence(evidence string) string {
	evidence = strings.TrimSpace(evidence)
	evidence = strings.TrimRight(evidence, ". ")
	return evidence
}

func hasExplicitEvidenceDate(evidence string) bool {
	return explicitEvidenceDateRE.MatchString(evidence)
}

// parseEntries splits a raw section body (no header lines) into individual entry blocks.
// Each entry is the text between blank-line separators. Returns one block per valid entry.
func parseEntries(body string) []string {
	raw := strings.Split(body, "\n\n")
	var entries []string
	for _, block := range raw {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
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

type parsedMemory struct {
	Intro    string
	Sections map[string]string
}

func parseMemory(content string) parsedMemory {
	parsed := parsedMemory{Sections: make(map[string]string)}
	body := memoryBody(content)

	var introLines []string
	var currentCat string
	var buf strings.Builder

	flush := func() {
		if currentCat != "" {
			parsed.Sections[currentCat] = buf.String()
			buf.Reset()
		}
	}

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			if currentCat == "" {
				parsed.Intro = strings.TrimSpace(strings.Join(introLines, "\n"))
			} else {
				flush()
			}
			currentCat = normalizeCategory(strings.TrimPrefix(line, "## "))
			continue
		}

		if currentCat == "" {
			introLines = append(introLines, line)
			continue
		}
		buf.WriteString(line + "\n")
	}

	if currentCat == "" {
		parsed.Intro = strings.TrimSpace(strings.Join(introLines, "\n"))
	} else {
		flush()
	}

	return parsed
}

func memoryBody(content string) string {
	body := content
	if strings.HasPrefix(content, "# User Memory") {
		idx := strings.Index(content, "\n")
		if idx >= 0 {
			body = content[idx+1:]
		} else {
			body = ""
		}
	}
	return strings.TrimLeft(body, "\n")
}

// parseSections parses a categorized user memory file into a map of
// category -> section body (the text under each ## heading, excluding the heading itself).
func parseSections(content string) map[string]string {
	return parseMemory(content).Sections
}

// serializeMemory writes the intro block and category map back to a full file string.
func serializeMemory(intro string, sections map[string]string) string {
	var sb strings.Builder
	sb.WriteString("# User Memory\n")
	if strings.TrimSpace(intro) != "" {
		sb.WriteString("\n")
		sb.WriteString(strings.TrimSpace(intro))
		sb.WriteString("\n")
	}
	for _, cat := range ValidCategories {
		body, ok := sections[cat]
		if !ok || strings.TrimSpace(body) == "" {
			continue
		}
		sb.WriteString("\n## ")
		sb.WriteString(displayCategoryName(cat))
		sb.WriteString("\n\n")
		sb.WriteString(strings.TrimSpace(body))
		sb.WriteString("\n")
	}
	return sb.String()
}

// serializeSections writes only the category map back to a full file string.
func serializeSections(sections map[string]string) string {
	return serializeMemory("", sections)
}

// migrateIfNeeded detects files in the old flat format (no ## category headers)
// and migrates all their entries into the ## Notes section.
// Files already in the categorized format are returned unchanged.
func migrateIfNeeded(content string) string {
	if content == "" {
		return content
	}
	if !needsMigration(content) {
		return content
	}

	// Old format: parse flat entries and move them all to "notes".
	entries := parseEntries(memoryBody(content))
	if len(entries) == 0 {
		return "# User Memory\n"
	}

	sections := map[string]string{
		"notes": strings.Join(entries, "\n\n") + "\n",
	}
	return serializeSections(sections)
}

// replaceOrAppendCategorized stores entry in the category section of content.
// If an entry with a matching statement (case-insensitive) exists anywhere in the
// file, it is replaced in place (regardless of which section it lives in).
// Otherwise the entry is appended to the given category section.
func replaceOrAppendCategorized(content, statement, newEntry, cat string) string {
	parsed := parseMemory(content)
	sections := parsed.Sections

	// Search all sections for an existing matching statement.
	for secCat, body := range sections {
		entries := parseEntries(body)
		for i, e := range entries {
			if strings.EqualFold(statementOf(e), strings.TrimSpace(statement)) {
				entries[i] = strings.TrimSpace(newEntry)
				sections[secCat] = strings.Join(entries, "\n\n") + "\n"
				return serializeMemory(parsed.Intro, sections)
			}
		}
	}

	// Not found — append to the target category.
	existing := strings.TrimSpace(sections[cat])
	if existing == "" {
		sections[cat] = strings.TrimSpace(newEntry) + "\n"
	} else {
		sections[cat] = existing + "\n\n" + strings.TrimSpace(newEntry) + "\n"
	}
	return serializeMemory(parsed.Intro, sections)
}

// deleteCategorizedEntry removes the entry with a matching statement from any
// category section. Returns the updated file content.
func deleteCategorizedEntry(content, statement string) string {
	parsed := parseMemory(content)
	sections := parsed.Sections
	for cat, body := range sections {
		entries := parseEntries(body)
		kept := entries[:0]
		for _, e := range entries {
			if !strings.EqualFold(statementOf(e), strings.TrimSpace(statement)) {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			sections[cat] = ""
		} else {
			sections[cat] = strings.Join(kept, "\n\n") + "\n"
		}
	}
	return serializeMemory(parsed.Intro, sections)
}

func mergeCategorizedContent(primary, secondary string) string {
	primaryContent := migrateIfNeeded(primary)
	secondaryContent := migrateIfNeeded(secondary)
	primaryParsed := parseMemory(primaryContent)
	secondaryParsed := parseMemory(secondaryContent)
	primarySections := primaryParsed.Sections
	secondarySections := secondaryParsed.Sections
	mergedSections := make(map[string]string, len(ValidCategories))

	for _, cat := range ValidCategories {
		entries := make([]string, 0)
		seen := make(map[string]struct{})

		for _, block := range parseEntries(primarySections[cat]) {
			statement := strings.ToLower(statementOf(block))
			if _, ok := seen[statement]; ok {
				continue
			}
			seen[statement] = struct{}{}
			entries = append(entries, strings.TrimSpace(block))
		}
		for _, block := range parseEntries(secondarySections[cat]) {
			statement := strings.ToLower(statementOf(block))
			if _, ok := seen[statement]; ok {
				continue
			}
			seen[statement] = struct{}{}
			entries = append(entries, strings.TrimSpace(block))
		}

		if len(entries) == 0 {
			continue
		}
		sort.SliceStable(entries, func(i, j int) bool {
			return strings.ToLower(statementOf(entries[i])) < strings.ToLower(statementOf(entries[j]))
		})
		mergedSections[cat] = strings.Join(entries, "\n\n") + "\n"
	}

	if len(mergedSections) == 0 {
		return ""
	}
	intro := primaryParsed.Intro
	if strings.TrimSpace(intro) == "" {
		intro = secondaryParsed.Intro
	}
	return serializeMemory(intro, mergedSections)
}
