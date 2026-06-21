package usermemory

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// ValidCategories lists the supported memory categories in display order.
// The model can assign any of these when storing a fact.
var ValidCategories = []string{"identity", "system_rules", "preferences", "notes"}

// defaultCategory is used when the model omits the category argument.
const defaultCategory = "notes"

// Store manages persistent per-user memory in SQLite.
// Facts are rendered through the same Markdown format that the agent and tools
// used before the SQLite migration:
//
//	## Identity
//
//	The user's name is Alex.
//
//	- Evidence: User stated "my name is Alex". Date: [2026-04-04].
type Store struct {
	dbPath    string
	legacyDir string
	db        *database.DB
	sql       *sql.DB
	log       *config.Logger

	speakerLineResolver func(string) (string, error)
	embedder            llm.Embedder
	embeddingModel      string
	embeddingDimension  int
	vectorTableReady    bool
}

// NewStore creates a SQLite-backed Store. It is retained for tests and callers
// that do not need embedding support or legacy markdown import.
func NewStore(basedir string, log *config.Logger) *Store {
	store, err := NewSQLiteStore(basedir, "", nil, "", log)
	if err != nil {
		panic(err)
	}
	return store
}

// NewSQLiteStore creates a SQLite-backed Store.
func NewSQLiteStore(dbPath, legacyDir string, embedder llm.Embedder, embeddingModel string, log *config.Logger) (*Store, error) {
	db, err := database.Open(dbPath, log)
	if err != nil {
		return nil, err
	}
	store := &Store{
		dbPath:         dbPath,
		legacyDir:      legacyDir,
		db:             db,
		sql:            db.SQL(),
		log:            log,
		embedder:       embedder,
		embeddingModel: strings.TrimSpace(embeddingModel),
	}
	return store, nil
}

// Close closes the store's database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// MigrateLegacyMarkdown imports legacy per-user Markdown files into SQLite.
// The source files are intentionally left untouched as backups.
func (s *Store) MigrateLegacyMarkdown() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.MigrateLegacyUserMemory(s.legacyDir)
}

// BackfillEmbeddings stores vectors for existing memory entries that do not
// have one yet. Text memory remains usable if individual embeddings fail.
func (s *Store) BackfillEmbeddings(ctx context.Context) error {
	if s == nil || s.embedder == nil || s.embeddingModel == "" {
		return nil
	}

	entries, err := s.entriesNeedingEmbeddings()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	stored := 0
	failed := 0
	for _, entry := range entries {
		if err := s.storeEmbedding(ctx, entry.ID, entry.Category, entry.Statement, entry.Evidence); err != nil {
			failed++
			s.log.Warn("memory.user.embedding_backfill_failed", "failed to backfill user memory embedding", config.F("entry_id", entry.ID), config.F("user_id", entry.UserID), config.F("category", entry.Category), config.F("status", "degraded"), config.ErrorField(err))
			continue
		}
		stored++
	}
	s.log.Info("memory.user.embedding_backfilled", "backfilled user memory embeddings", config.F("embedding_model", s.embeddingModel), config.F("embedding_dimension_count", s.embeddingDimension), config.F("entry_count", len(entries)), config.F("stored_count", stored), config.F("failed_count", failed), config.F("status", "ok"))
	return nil
}

// SetSpeakerLineResolver configures how speaker intro lines are derived.
func (s *Store) SetSpeakerLineResolver(resolver func(string) (string, error)) {
	s.speakerLineResolver = resolver
}

// Read returns the full rendered Markdown contents of the user's memory.
// Returns an empty string if no memory exists yet.
func (s *Store) Read(userID string) (string, error) {
	intro, sections, err := s.readStructured(userID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(intro) == "" && len(sections) == 0 {
		return "", nil
	}
	return serializeMemory(intro, sections), nil
}

// ReadIntro returns the top intro block from the user's memory profile.
func (s *Store) ReadIntro(userID string) (string, error) {
	var intro string
	err := s.sql.QueryRow(`SELECT intro FROM user_memory_profiles WHERE canonical_user_id = ?`, userID).Scan(&intro)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read user memory intro for %q: %w", userID, err)
	}
	intro = strings.TrimSpace(intro)
	if !strings.HasPrefix(intro, "You are speaking with ") {
		return "", nil
	}
	return intro, nil
}

// SyncSpeakerIntro creates or updates the user's memory profile intro.
func (s *Store) SyncSpeakerIntro(userID, intro string) error {
	if err := s.ensureAccountUser(userID); err != nil {
		return err
	}
	now := formatMemoryTime(time.Now().UTC())
	if _, err := s.sql.Exec(`
INSERT INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET intro = excluded.intro, updated_at = excluded.updated_at
`, userID, strings.TrimSpace(intro), now, now); err != nil {
		return fmt.Errorf("failed to sync user memory intro for %q: %w", userID, err)
	}

	s.log.Debug("memory.user.synced_speaker_intro", "synced user speaker intro", config.F("user_id", userID))
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
	return s.SetWithContext(context.Background(), userID, statement, evidence, category)
}

// SetWithContext stores a fact and updates its embedding when embedding support is configured.
func (s *Store) SetWithContext(ctx context.Context, userID, statement, evidence, category string) error {
	if err := s.ensureAccountUser(userID); err != nil {
		return err
	}
	if s.speakerLineResolver != nil {
		intro, err := s.speakerLineResolver(userID)
		if err != nil {
			return fmt.Errorf("failed to resolve speaker line for %q: %w", userID, err)
		}
		if strings.TrimSpace(intro) != "" {
			if err := s.SyncSpeakerIntro(userID, intro); err != nil {
				return err
			}
		}
	}
	cat := normalizeCategory(category)
	evidence = sanitizeEvidence(evidence)
	statement = strings.TrimSpace(statement)
	now := formatMemoryTime(time.Now().UTC())
	res, err := s.sql.Exec(`
INSERT INTO user_memory_entries (canonical_user_id, category, statement, statement_key, evidence, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, statement_key) DO UPDATE SET
	category = excluded.category,
	statement = excluded.statement,
	evidence = excluded.evidence,
	updated_at = excluded.updated_at
`, userID, cat, statement, statementKey(statement), evidence, now, now)
	if err != nil {
		return fmt.Errorf("failed to write user memory for %q: %w", userID, err)
	}
	id, err := res.LastInsertId()
	if err != nil || id == 0 {
		if err := s.sql.QueryRow(`SELECT id FROM user_memory_entries WHERE canonical_user_id = ? AND statement_key = ?`, userID, statementKey(statement)).Scan(&id); err != nil {
			return fmt.Errorf("failed to read stored user memory id for %q: %w", userID, err)
		}
	}
	if err := s.storeEmbedding(ctx, id, cat, statement, evidence); err != nil {
		s.log.Warn("memory.user.embedding_failed", "failed to store user memory embedding", config.F("user_id", userID), config.F("category", cat), config.F("status", "degraded"), config.ErrorField(err))
	}

	s.log.Debug("memory.user.stored", "stored user memory", config.F("user_id", userID), config.F("category", cat))
	return nil
}

// ReadCategory returns only the facts stored under the given category section.
// Returns an empty string if the section does not exist or the file is missing.
func (s *Store) ReadCategory(userID, category string) (string, error) {
	_, sections, err := s.readStructured(userID)
	if err != nil {
		return "", err
	}
	cat := normalizeCategory(category)
	content, ok := sections[cat]
	if !ok || strings.TrimSpace(content) == "" {
		return "", nil
	}
	return "## " + displayCategoryName(cat) + "\n\n" + strings.TrimSpace(content) + "\n", nil
}

// Search returns semantically relevant memories for a query when vectors are available.
func (s *Store) Search(ctx context.Context, userID, category, query string, limit int) (string, error) {
	if s.embedder == nil || s.embeddingModel == "" {
		return "", fmt.Errorf("semantic user memory recall is not configured")
	}
	if !s.vectorTableExists() {
		return "", fmt.Errorf("semantic user memory vector table is not initialized")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}
	resp, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: s.embeddingModel, Input: strings.TrimSpace(query)})
	if err != nil {
		return "", err
	}
	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return "", fmt.Errorf("embedding response contained no vector")
	}
	vector := float64ToFloat32(resp.Embeddings[0])
	serialized, err := sqlite_vec.SerializeFloat32(vector)
	if err != nil {
		return "", fmt.Errorf("failed to serialize user memory query embedding: %w", err)
	}

	cat := normalizeCategory(category)
	var rows *sql.Rows
	if strings.TrimSpace(category) == "" {
		rows, err = s.sql.Query(`
SELECT e.category, e.statement, e.evidence
FROM user_memory_vectors v
JOIN user_memory_entries e ON e.id = v.rowid
WHERE v.embedding MATCH ? AND e.canonical_user_id = ?
AND v.k = ?
ORDER BY v.distance
LIMIT ?
`, serialized, userID, limit, limit)
	} else {
		rows, err = s.sql.Query(`
SELECT e.category, e.statement, e.evidence
FROM user_memory_vectors v
JOIN user_memory_entries e ON e.id = v.rowid
WHERE v.embedding MATCH ? AND e.canonical_user_id = ? AND e.category = ?
AND v.k = ?
ORDER BY v.distance
LIMIT ?
`, serialized, userID, cat, limit, limit)
	}
	if err != nil {
		return "", fmt.Errorf("failed to search user memory vectors: %w", err)
	}
	defer rows.Close()

	sections := make(map[string][]string)
	for rows.Next() {
		var rowCat, statement, evidence string
		if err := rows.Scan(&rowCat, &statement, &evidence); err != nil {
			return "", fmt.Errorf("failed to scan user memory search result: %w", err)
		}
		sections[normalizeCategory(rowCat)] = append(sections[normalizeCategory(rowCat)], formatEntry(statement, evidence))
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("failed to read user memory search results: %w", err)
	}
	if len(sections) == 0 {
		return "", nil
	}
	rendered := make(map[string]string)
	for _, cat := range ValidCategories {
		if entries := sections[cat]; len(entries) > 0 {
			rendered[cat] = strings.Join(entries, "\n")
		}
	}
	return serializeMemory("", rendered), nil
}

// Delete removes the entry whose statement matches the given text.
// The search spans all category sections. Returns nil if the file or entry does not exist.
func (s *Store) Delete(userID, statement string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin user memory delete: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	rows, err := tx.Query(`SELECT id FROM user_memory_entries WHERE canonical_user_id = ? AND statement_key = ?`, userID, statementKey(statement))
	if err != nil {
		return fmt.Errorf("failed to read user memory entries for delete: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan user memory entry for delete: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("failed to close user memory delete rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read user memory entries for delete: %w", err)
	}
	for _, id := range ids {
		if err := s.deleteVectorTx(tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_entries WHERE canonical_user_id = ? AND statement_key = ?`, userID, statementKey(statement)); err != nil {
		return fmt.Errorf("failed to delete user memory for %q: %w", userID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit user memory delete: %w", err)
	}

	s.log.Debug("memory.user.deleted_entry", "deleted user memory entry", config.F("user_id", userID))
	return nil
}

// DeleteAll removes the user's entire memory file.
// Returns nil if the file does not exist.
func (s *Store) DeleteAll(userID string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin user memory wipe: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	if err := s.deleteUserVectorsTx(tx, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_entries WHERE canonical_user_id = ?`, userID); err != nil {
		return fmt.Errorf("failed to delete user memory entries for %q: %w", userID, err)
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_profiles WHERE canonical_user_id = ?`, userID); err != nil {
		return fmt.Errorf("failed to delete user memory profile for %q: %w", userID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit user memory wipe: %w", err)
	}

	s.log.Debug("memory.user.deleted_all", "deleted all user memory", config.F("user_id", userID))
	return nil
}

// WriteFull atomically replaces the entire content of the user's memory file.
// It is used by the LLM migration path to persist a freshly categorized file.
func (s *Store) WriteFull(userID, content string) error {
	content, err := s.withSpeakerIntro(userID, content)
	if err != nil {
		return err
	}
	parsed := parseMemory(migrateIfNeeded(content))
	if err := s.replaceStructured(userID, parsed.Intro, parsed.Sections); err != nil {
		return err
	}
	s.log.Debug("memory.user.file_written", "wrote full user memory file", config.F("user_id", userID), config.F("content_chars", len(content)))
	return nil
}

// MergeUsers merges the persistent memory file for loserUserID into winnerUserID.
// Statement lines are de-duplicated case-insensitively, preserving the winner's
// existing entry when a duplicate exists in both files.
func (s *Store) MergeUsers(winnerUserID, loserUserID string) error {
	if winnerUserID == "" || loserUserID == "" || winnerUserID == loserUserID {
		return nil
	}
	if err := s.ensureAccountUser(winnerUserID); err != nil {
		return err
	}
	winnerIntro, err := s.readRawIntro(winnerUserID)
	if err != nil {
		return err
	}
	loserIntro, err := s.readRawIntro(loserUserID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(winnerIntro) == "" && strings.TrimSpace(loserIntro) != "" {
		if err := s.SyncSpeakerIntro(winnerUserID, loserIntro); err != nil {
			return err
		}
	}

	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin user memory merge: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	now := formatMemoryTime(time.Now().UTC())
	rows, err := tx.Query(`SELECT category, statement, statement_key, evidence FROM user_memory_entries WHERE canonical_user_id = ? ORDER BY category, statement_key`, loserUserID)
	if err != nil {
		return fmt.Errorf("failed to read source user memory for merge: %w", err)
	}
	type mergeEntry struct {
		category     string
		statement    string
		statementKey string
		evidence     string
	}
	var entries []mergeEntry
	for rows.Next() {
		var entry mergeEntry
		if err := rows.Scan(&entry.category, &entry.statement, &entry.statementKey, &entry.evidence); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan source user memory for merge: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("failed to close source user memory merge rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read source user memory for merge: %w", err)
	}
	stmt, err := tx.Prepare(`
INSERT OR IGNORE INTO user_memory_entries (canonical_user_id, category, statement, statement_key, evidence, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
`)
	if err != nil {
		return fmt.Errorf("failed to prepare user memory merge: %w", err)
	}
	defer stmt.Close()
	for _, entry := range entries {
		if _, err := stmt.Exec(winnerUserID, entry.category, entry.statement, entry.statementKey, entry.evidence, now, now); err != nil {
			return fmt.Errorf("failed to merge user memory entry: %w", err)
		}
	}
	if err := s.deleteUserVectorsTx(tx, loserUserID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_entries WHERE canonical_user_id = ?`, loserUserID); err != nil {
		return fmt.Errorf("failed to remove merged user memory entries: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_profiles WHERE canonical_user_id = ?`, loserUserID); err != nil {
		return fmt.Errorf("failed to remove merged user memory profile: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit user memory merge: %w", err)
	}

	s.log.Debug("memory.user.merged", "merged user memory", config.F("source_user_id", loserUserID), config.F("target_user_id", winnerUserID))
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

type memoryRow struct {
	ID        int64
	UserID    string
	Category  string
	Statement string
	Evidence  string
}

func (s *Store) readStructured(userID string) (string, map[string]string, error) {
	sections := make(map[string]string)
	intro, err := s.readRawIntro(userID)
	if err != nil {
		return "", nil, err
	}
	rows, err := s.sql.Query(`SELECT category, statement, evidence FROM user_memory_entries WHERE canonical_user_id = ? ORDER BY category, statement_key`, userID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read user memory for %q: %w", userID, err)
	}
	defer rows.Close()
	byCategory := make(map[string][]string)
	for rows.Next() {
		var cat, statement, evidence string
		if err := rows.Scan(&cat, &statement, &evidence); err != nil {
			return "", nil, fmt.Errorf("failed to scan user memory for %q: %w", userID, err)
		}
		byCategory[normalizeCategory(cat)] = append(byCategory[normalizeCategory(cat)], formatEntry(statement, evidence))
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("failed to read user memory for %q: %w", userID, err)
	}
	for _, cat := range ValidCategories {
		if entries := byCategory[cat]; len(entries) > 0 {
			sections[cat] = strings.Join(entries, "\n")
		}
	}
	return intro, sections, nil
}

func (s *Store) readRawIntro(userID string) (string, error) {
	var intro string
	err := s.sql.QueryRow(`SELECT intro FROM user_memory_profiles WHERE canonical_user_id = ?`, userID).Scan(&intro)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read user memory profile for %q: %w", userID, err)
	}
	return strings.TrimSpace(intro), nil
}

func (s *Store) replaceStructured(userID, intro string, sections map[string]string) error {
	if err := s.ensureAccountUser(userID); err != nil {
		return err
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin user memory write: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	if err := s.deleteUserVectorsTx(tx, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_entries WHERE canonical_user_id = ?`, userID); err != nil {
		return fmt.Errorf("failed to replace user memory entries for %q: %w", userID, err)
	}
	now := formatMemoryTime(time.Now().UTC())
	if _, err := tx.Exec(`
INSERT INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET intro = excluded.intro, updated_at = excluded.updated_at
`, userID, strings.TrimSpace(intro), now, now); err != nil {
		return fmt.Errorf("failed to replace user memory profile for %q: %w", userID, err)
	}
	stmt, err := tx.Prepare(`INSERT INTO user_memory_entries (canonical_user_id, category, statement, statement_key, evidence, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("failed to prepare user memory replacement: %w", err)
	}
	defer stmt.Close()
	for _, cat := range ValidCategories {
		for _, entry := range parseEntries(sections[cat]) {
			statement := statementOf(entry)
			evidence := evidenceOf(entry)
			if statement == "" {
				continue
			}
			if _, err := stmt.Exec(userID, cat, statement, statementKey(statement), evidence, now, now); err != nil {
				return fmt.Errorf("failed to replace user memory entry for %q: %w", userID, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit user memory replacement: %w", err)
	}
	return nil
}

func (s *Store) ensureAccountUser(userID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user memory: user id is required")
	}
	now := formatMemoryTime(time.Now().UTC())
	if _, err := s.sql.Exec(`INSERT OR IGNORE INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, now, now); err != nil {
		return fmt.Errorf("failed to ensure account user %q: %w", userID, err)
	}
	return nil
}

func (s *Store) entriesNeedingEmbeddings() ([]memoryRow, error) {
	rows, err := s.sql.Query(`SELECT id, canonical_user_id, category, statement, evidence FROM user_memory_entries ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to read user memory entries for embedding backfill: %w", err)
	}
	defer rows.Close()

	entries := make([]memoryRow, 0)
	for rows.Next() {
		var entry memoryRow
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Category, &entry.Statement, &entry.Evidence); err != nil {
			return nil, fmt.Errorf("failed to scan user memory entry for embedding backfill: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read user memory entries for embedding backfill: %w", err)
	}
	if len(entries) == 0 || !s.vectorTableReady || !s.vectorTableExists() {
		return entries, nil
	}

	vectorRows, err := s.sql.Query(`SELECT rowid FROM user_memory_vectors`)
	if err != nil {
		return nil, fmt.Errorf("failed to read user memory vector rows for embedding backfill: %w", err)
	}
	defer vectorRows.Close()
	existing := make(map[int64]struct{})
	for vectorRows.Next() {
		var id int64
		if err := vectorRows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan user memory vector row for embedding backfill: %w", err)
		}
		existing[id] = struct{}{}
	}
	if err := vectorRows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read user memory vector rows for embedding backfill: %w", err)
	}

	missing := entries[:0]
	for _, entry := range entries {
		if _, ok := existing[entry.ID]; !ok {
			missing = append(missing, entry)
		}
	}
	return missing, nil
}

func (s *Store) storeEmbedding(ctx context.Context, id int64, category, statement, evidence string) error {
	if s.embedder == nil || s.embeddingModel == "" {
		return nil
	}
	resp, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: s.embeddingModel, Input: embeddingText(category, statement, evidence)})
	if err != nil {
		return err
	}
	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return fmt.Errorf("embedding response contained no vector")
	}
	vector := float64ToFloat32(resp.Embeddings[0])
	if err := s.ensureVectorTable(len(vector)); err != nil {
		return err
	}
	serialized, err := sqlite_vec.SerializeFloat32(vector)
	if err != nil {
		return fmt.Errorf("failed to serialize user memory embedding: %w", err)
	}
	if _, err := s.sql.Exec(`INSERT OR REPLACE INTO user_memory_vectors(rowid, embedding) VALUES (?, ?)`, id, serialized); err != nil {
		return fmt.Errorf("failed to store user memory vector: %w", err)
	}
	return nil
}

func (s *Store) ensureVectorTable(dimension int) error {
	if dimension <= 0 {
		return fmt.Errorf("embedding dimension must be positive")
	}
	if !s.vectorTableReady || s.embeddingDimension != dimension {
		if _, err := s.sql.Exec(`DROP TABLE IF EXISTS user_memory_vectors`); err != nil {
			return fmt.Errorf("failed to drop stale user memory vector table: %w", err)
		}
		if _, err := s.sql.Exec(fmt.Sprintf(`CREATE VIRTUAL TABLE user_memory_vectors USING vec0(embedding float[%d])`, dimension)); err != nil {
			return fmt.Errorf("failed to initialize user memory vector table: %w", err)
		}
		s.embeddingDimension = dimension
		s.vectorTableReady = true
		return nil
	}
	if _, err := s.sql.Exec(fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS user_memory_vectors USING vec0(embedding float[%d])`, dimension)); err != nil {
		return fmt.Errorf("failed to initialize user memory vector table: %w", err)
	}
	return nil
}

func (s *Store) vectorTableExists() bool {
	var name string
	err := s.sql.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'user_memory_vectors'`).Scan(&name)
	return err == nil
}

func (s *Store) deleteVectorTx(tx *sql.Tx, id int64) error {
	if !s.vectorTableExists() {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_vectors WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("failed to delete user memory vector: %w", err)
	}
	return nil
}

func (s *Store) deleteUserVectorsTx(tx *sql.Tx, userID string) error {
	if !s.vectorTableExists() {
		return nil
	}
	rows, err := tx.Query(`SELECT id FROM user_memory_entries WHERE canonical_user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("failed to read user memory vectors for delete: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("failed to scan user memory vector for delete: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM user_memory_vectors WHERE rowid = ?`, id); err != nil {
			return fmt.Errorf("failed to delete user memory vector: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read user memory vectors for delete: %w", err)
	}
	return nil
}

func formatMemoryTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func statementKey(statement string) string {
	return strings.ToLower(strings.TrimSpace(statement))
}

func evidenceOf(entry string) string {
	lines := strings.SplitN(strings.TrimSpace(entry), "\n", 2)
	if len(lines) != 2 {
		return ""
	}
	evidence := strings.TrimSpace(lines[1])
	evidence = strings.TrimPrefix(evidence, "- Evidence:")
	return sanitizeEvidence(evidence)
}

func embeddingText(category, statement, evidence string) string {
	return fmt.Sprintf("%s\n%s\nEvidence: %s", normalizeCategory(category), strings.TrimSpace(statement), strings.TrimSpace(evidence))
}

func float64ToFloat32(values []float64) []float32 {
	out := make([]float32, 0, len(values))
	for _, value := range values {
		out = append(out, float32(value))
	}
	return out
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
	raw := strings.Split(strings.TrimSpace(body), "\n\n")
	var entries []string
	for i := 0; i < len(raw); i++ {
		block := raw[i]
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.SplitN(block, "\n", 2)
		if len(lines) == 2 && strings.HasPrefix(strings.TrimSpace(lines[1]), "- Evidence:") {
			entries = append(entries, block)
			continue
		}
		if i+1 < len(raw) && strings.HasPrefix(strings.TrimSpace(raw[i+1]), "- Evidence:") {
			entries = append(entries, block+"\n"+strings.TrimSpace(raw[i+1]))
			i++
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

// MemoryEntry is a structured fact parsed from markdown memory content.
type MemoryEntry struct {
	Statement string `json:"statement"`
	Evidence  string `json:"evidence"`
}

// ParsedContent is a structured view of a user memory markdown document.
type ParsedContent struct {
	Intro    string                   `json:"intro,omitempty"`
	Sections map[string][]MemoryEntry `json:"sections,omitempty"`
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

// ParseContent converts persisted memory markdown into structured sections and entries.
func ParseContent(content string) ParsedContent {
	parsed := parseMemory(content)
	out := ParsedContent{
		Intro:    strings.TrimSpace(parsed.Intro),
		Sections: make(map[string][]MemoryEntry),
	}

	for _, cat := range ValidCategories {
		body, ok := parsed.Sections[cat]
		if !ok {
			continue
		}
		entries := parseEntries(body)
		if len(entries) == 0 {
			continue
		}
		out.Sections[cat] = make([]MemoryEntry, 0, len(entries))
		for _, entry := range entries {
			lines := strings.SplitN(strings.TrimSpace(entry), "\n", 2)
			memoryEntry := MemoryEntry{Statement: strings.TrimSpace(lines[0])}
			if len(lines) == 2 {
				evidence := strings.TrimSpace(lines[1])
				evidence = strings.TrimPrefix(evidence, "- Evidence:")
				memoryEntry.Evidence = strings.TrimSpace(evidence)
			}
			out.Sections[cat] = append(out.Sections[cat], memoryEntry)
		}
	}

	if len(out.Sections) == 0 {
		out.Sections = nil
	}
	return out
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
