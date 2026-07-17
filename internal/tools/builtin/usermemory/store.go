package usermemory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

const (
	ScopeShortTerm = "short_term"
	ScopeLongTerm  = "long_term"

	StatusActive     = "active"
	StatusExpired    = "expired"
	StatusSuperseded = "superseded"
	StatusDeleted    = "deleted"

	DefaultShortTermTTL = 30 * 24 * time.Hour
)

// ValidCategories lists supported memory categories in display order.
var ValidCategories = []string{"identity", "system_rules", "communication_preferences", "durable_preferences", "projects", "relationships", "environment", "notes"}

// MemoryEntry is a single short-term or long-term user memory.
type MemoryEntry struct {
	ID              int64
	UserID          string
	Scope           string
	Category        string
	Statement       string
	Evidence        string
	Confidence      float64
	Importance      int
	Status          string
	SourceSessionID string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastUsedAt      time.Time
	ExpiresAt       time.Time
	SupersedesID    int64
	EmbeddingModel  string
	EmbeddingDim    int
	Score           float64
}

// SessionTurn is a completed exchange stored for session continuity.
type SessionTurn struct {
	ID            int64
	SessionID     string
	UserID        string
	UserText      string
	AssistantText string
	ToolNames     []string
	Importance    int
	TopicTags     []string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Score         float64
}

// ContextOptions controls request-time memory retrieval.
type ContextOptions struct {
	RecentTurns int

	ContextBudgetChars int
}

// RetrievedContext contains the memory block selected for a request.
type RetrievedContext struct {
	Block           string
	RecentTurnCount int
}

// SaveRequest describes a user memory write.
type SaveRequest struct {
	Scope           string
	Category        string
	Statement       string
	Evidence        string
	Confidence      float64
	Importance      int
	SourceSessionID string
	TTL             time.Duration
	Supersedes      string
	Embedding       []float64
}

// Store manages speaker profiles, user memories, and session memory in SQLite.
type Store struct {
	dbPath     string
	db         *database.DB
	sql        *sql.DB
	log        *config.Logger
	embedder   llm.Embedder
	embedModel string

	speakerLineResolver func(string) (string, error)
}

// NewStore creates a SQLite-backed Store. The argument is treated as a database path.
func NewStore(dbPath string, log *config.Logger) *Store {
	store, err := NewSQLiteStore(dbPath, nil, "", log)
	if err != nil {
		panic(err)
	}
	return store
}

// NewSQLiteStore creates a fresh-schema SQLite-backed Store.
func NewSQLiteStore(dbPath string, embedder llm.Embedder, embeddingModel string, log *config.Logger) (*Store, error) {
	db, err := database.Open(dbPath, log)
	if err != nil {
		return nil, err
	}
	return &Store{
		dbPath:     dbPath,
		db:         db,
		sql:        db.SQL(),
		log:        log,
		embedder:   embedder,
		embedModel: strings.TrimSpace(embeddingModel),
	}, nil
}

// Close closes the store database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SetSpeakerLineResolver configures how speaker intro lines are derived.
func (s *Store) SetSpeakerLineResolver(resolver func(string) (string, error)) {
	s.speakerLineResolver = resolver
}

// SyncSpeakerIntro creates or updates the account-derived speaker intro.
func (s *Store) SyncSpeakerIntro(userID, intro string) error {
	if err := s.ensureAccountUser(userID); err != nil {
		return err
	}
	now := formatTime(time.Now())
	_, err := s.sql.Exec(`
INSERT INTO user_memory_profiles (canonical_user_id, intro, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(canonical_user_id) DO UPDATE SET intro = excluded.intro, updated_at = excluded.updated_at
`, userID, strings.TrimSpace(intro), now, now)
	if err != nil {
		return fmt.Errorf("failed to sync user memory intro for %q: %w", userID, err)
	}
	return nil
}

// ReadIntro returns the current speaker intro for a user.
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

// MergeUsers moves memory/session ownership from loserUserID into winnerUserID.
func (s *Store) MergeUsers(winnerUserID, loserUserID string) error {
	if winnerUserID == "" || loserUserID == "" || winnerUserID == loserUserID {
		return nil
	}
	if err := s.ensureAccountUser(winnerUserID); err != nil {
		return err
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin memory merge: %w", err)
	}
	defer tx.Rollback() // nolint:errcheck
	for _, stmt := range []string{
		`UPDATE memory_entries SET canonical_user_id = ? WHERE canonical_user_id = ?`,
		`UPDATE session_turns SET canonical_user_id = ? WHERE canonical_user_id = ?`,
	} {
		if _, err := tx.Exec(stmt, winnerUserID, loserUserID); err != nil {
			return fmt.Errorf("failed to merge memory users: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM user_memory_profiles WHERE canonical_user_id = ?`, loserUserID); err != nil {
		return fmt.Errorf("failed to remove merged memory profile: %w", err)
	}
	return tx.Commit()
}

// SaveMemory creates or updates a scoped memory entry.
func (s *Store) SaveMemory(ctx context.Context, userID string, req SaveRequest) (MemoryEntry, error) {
	if err := s.ensureAccountUser(userID); err != nil {
		return MemoryEntry{}, err
	}
	statement := strings.TrimSpace(req.Statement)
	if statement == "" {
		return MemoryEntry{}, fmt.Errorf("memory statement is required")
	}
	evidence := strings.TrimSpace(req.Evidence)
	if evidence == "" {
		evidence = "Stored from user interaction"
	}
	scope := normalizeScope(req.Scope)
	category := normalizeCategory(req.Category)
	importance := clampInt(req.Importance, 1, 5, 3)
	confidence := req.Confidence
	if confidence <= 0 || confidence > 1 {
		confidence = 0.8
	}
	now := time.Now().UTC()
	var expiresAt *time.Time
	if scope == ScopeShortTerm {
		ttl := req.TTL
		if ttl <= 0 {
			ttl = DefaultShortTermTTL
		}
		exp := now.Add(ttl).UTC()
		expiresAt = &exp
	}
	embedding := append([]float64(nil), req.Embedding...)
	if len(embedding) == 0 {
		embedding = s.embedBestEffort(ctx, memoryEmbeddingText(scope, category, statement, evidence))
	}
	embeddingModel := ""
	embeddingDim := 0
	if len(embedding) > 0 {
		embeddingModel = s.embedModel
		embeddingDim = len(embedding)
	}

	var supersedesID any
	if strings.TrimSpace(req.Supersedes) != "" {
		id, err := s.markSuperseded(userID, scope, req.Supersedes)
		if err != nil {
			return MemoryEntry{}, err
		}
		if id > 0 {
			supersedesID = id
		}
	}

	res, err := s.sql.Exec(`
INSERT INTO memory_entries (canonical_user_id, scope, category, statement, statement_key, evidence, confidence, importance, status, source_session_id, created_at, updated_at, expires_at, supersedes_id, embedding_model, embedding_dim)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(canonical_user_id, scope, statement_key) DO UPDATE SET
	category = excluded.category,
	statement = excluded.statement,
	evidence = excluded.evidence,
	confidence = excluded.confidence,
	importance = excluded.importance,
	status = 'active',
	source_session_id = excluded.source_session_id,
	updated_at = excluded.updated_at,
	expires_at = excluded.expires_at,
	supersedes_id = excluded.supersedes_id,
	embedding_model = excluded.embedding_model,
	embedding_dim = excluded.embedding_dim
`, userID, scope, category, statement, statementKey(statement), evidence, confidence, importance, strings.TrimSpace(req.SourceSessionID), formatTime(now), formatTime(now), nullableTime(expiresAt), supersedesID, embeddingModel, embeddingDim)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("failed to save memory for %q: %w", userID, err)
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		_ = s.sql.QueryRow(`SELECT id FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND statement_key = ?`, userID, scope, statementKey(statement)).Scan(&id)
	}
	if err := s.storeMemoryVector(id, embedding); err != nil {
		return MemoryEntry{}, err
	}
	entry, err := s.EntryByID(id)
	if err != nil {
		return MemoryEntry{}, err
	}
	s.recordEvent(entry.ID, "updated", "", req.SourceSessionID, "")
	return entry, nil
}

// EntryByID reads a memory entry by ID.
func (s *Store) EntryByID(id int64) (MemoryEntry, error) {
	rows, err := s.sql.Query(`SELECT id, canonical_user_id, scope, category, statement, evidence, confidence, importance, status, source_session_id, created_at, updated_at, last_used_at, expires_at, COALESCE(supersedes_id, 0), embedding_model, embedding_dim FROM memory_entries WHERE id = ?`, id)
	if err != nil {
		return MemoryEntry{}, err
	}
	defer rows.Close()
	if rows.Next() {
		return scanMemoryEntry(rows)
	}
	return MemoryEntry{}, sql.ErrNoRows
}

// Search returns active memories matching the requested filters.
func (s *Store) Search(ctx context.Context, userID, scope, category, query string, limit int) ([]MemoryEntry, error) {
	query = strings.TrimSpace(query)
	var queryVector []float64
	if query != "" {
		queryVector = s.embedBestEffort(ctx, query)
	}
	return s.searchWithVector(userID, scope, category, query, queryVector, limit)
}

func (s *Store) searchWithVector(userID, scope, category, query string, queryVector []float64, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		limit = 8
	}
	if limit > 25 {
		limit = 25
	}
	if err := s.expireOldMemories(); err != nil {
		return nil, err
	}
	normalizedScope := normalizeOptionalScope(scope)
	normalizedCategory := normalizeOptionalCategory(category)
	query = strings.TrimSpace(query)
	var entries []MemoryEntry
	if query != "" {
		if vectorEntries, ok, err := s.searchMemoryVectors(queryVector, userID, normalizedScope, normalizedCategory, limit); err != nil {
			return nil, err
		} else if ok {
			entries = vectorEntries
		} else {
			var err error
			entries, err = s.activeEntries(userID, normalizedScope, normalizedCategory)
			if err != nil || len(entries) == 0 {
				return entries, err
			}
			for i := range entries {
				if strings.Contains(strings.ToLower(entries[i].Statement), strings.ToLower(query)) {
					entries[i].Score = 0.55 + (float64(entries[i].Importance)/5)*0.20
				}
			}
			sort.SliceStable(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
		}
	} else {
		var err error
		entries, err = s.activeEntries(userID, normalizedScope, normalizedCategory)
		if err != nil || len(entries) == 0 {
			return entries, err
		}
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].Importance == entries[j].Importance {
				return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
			}
			return entries[i].Importance > entries[j].Importance
		})
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	ids := make([]any, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
		s.recordEvent(entry.ID, "retrieved", "", "", "")
	}
	if len(ids) > 0 {
		now := formatTime(time.Now())
		for _, id := range ids {
			_, _ = s.sql.Exec(`UPDATE memory_entries SET last_used_at = ? WHERE id = ?`, now, id)
		}
	}
	return entries, nil
}

// ListMemories returns active memories without semantic ranking.
func (s *Store) ListMemories(userID, scope, category string, limit int) ([]MemoryEntry, error) {
	return s.Search(context.Background(), userID, scope, category, "", limit)
}

// Forget marks one or more user memories deleted.
func (s *Store) Forget(userID, target, scope string) (int64, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return 0, fmt.Errorf("memory target is required")
	}
	if strings.EqualFold(target, "all") {
		res, err := s.sql.Exec(`UPDATE memory_entries SET status = 'deleted', updated_at = ? WHERE canonical_user_id = ? AND status = 'active'`, formatTime(time.Now()), userID)
		if err != nil {
			return 0, fmt.Errorf("failed to delete memories for %q: %w", userID, err)
		}
		return res.RowsAffected()
	}
	stmt := `UPDATE memory_entries SET status = 'deleted', updated_at = ? WHERE canonical_user_id = ? AND statement_key = ? AND status = 'active'`
	args := []any{formatTime(time.Now()), userID, statementKey(target)}
	if normalizeOptionalScope(scope) != "" {
		stmt += ` AND scope = ?`
		args = append(args, normalizeScope(scope))
	}
	res, err := s.sql.Exec(stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to delete memory for %q: %w", userID, err)
	}
	return res.RowsAffected()
}

// Read renders all active memories for compatibility with older callers/tests.
func (s *Store) Read(userID string) (string, error) {
	entries, err := s.ListMemories(userID, "", "", 100)
	if err != nil {
		return "", err
	}
	intro, _ := s.ReadIntro(userID)
	return RenderMemory(intro, entries), nil
}

// ReadCategory renders active memories for one category.
func (s *Store) ReadCategory(userID, category string) (string, error) {
	entries, err := s.ListMemories(userID, "", category, 100)
	if err != nil || len(entries) == 0 {
		return "", err
	}
	return RenderMemory("", entries), nil
}

// SetWithContext stores a long-term memory for compatibility with memory.save handlers.
func (s *Store) SetWithContext(ctx context.Context, userID, statement, evidence, category string) error {
	_, err := s.SaveMemory(ctx, userID, SaveRequest{Scope: ScopeLongTerm, Category: category, Statement: statement, Evidence: evidence, Confidence: 0.9, Importance: 3})
	return err
}

// Delete marks a specific memory deleted.
func (s *Store) Delete(userID, statement string) error {
	_, err := s.Forget(userID, statement, "")
	return err
}

// DeleteAll marks all user memories deleted.
func (s *Store) DeleteAll(userID string) error {
	_, err := s.Forget(userID, "all", "")
	return err
}

// AppendSessionTurn stores a completed session exchange.
func (s *Store) AppendSessionTurn(ctx context.Context, sessionID, userID, userText, assistantText string, toolNames []string, ttl time.Duration) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(assistantText) == "" {
		return nil
	}
	if err := s.ensureAccountUser(userID); err != nil {
		return err
	}
	now := time.Now().UTC()
	var expires *time.Time
	if ttl > 0 {
		exp := now.Add(ttl).UTC()
		expires = &exp
	}
	_, err := s.sql.Exec(`
INSERT INTO session_turns (session_id, canonical_user_id, user_text, assistant_text, tool_names, importance, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, sessionID, userID, strings.TrimSpace(userText), strings.TrimSpace(assistantText), strings.Join(uniqueStrings(toolNames), ","), 2, formatTime(now), nullableTime(expires))
	if err != nil {
		return fmt.Errorf("failed to append session turn: %w", err)
	}
	return nil
}

// RecentSessionTurns returns newest completed exchanges, newest first.
func (s *Store) RecentSessionTurns(sessionID string, offset int, count int) ([]SessionTurn, error) {
	if offset < 1 {
		offset = 1
	}
	if count < 1 {
		count = 1
	}
	if count > 10 {
		count = 10
	}
	if err := s.expireOldSessionTurns(); err != nil {
		return nil, err
	}
	rows, err := s.sql.Query(`
SELECT id, session_id, canonical_user_id, user_text, assistant_text, tool_names, importance, topic_tags, created_at, expires_at
FROM session_turns WHERE session_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?
`, sessionID, count, offset-1)
	if err != nil {
		return nil, fmt.Errorf("failed to read session turns: %w", err)
	}
	defer rows.Close()
	turns := []SessionTurn{}
	for rows.Next() {
		turn, err := scanSessionTurn(rows)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

// BuildContext retrieves and formats automatic session context for a request.
// Durable user memory is model-directed through memory.search.
func (s *Store) BuildContext(ctx context.Context, userID, sessionID, query string, opts ContextOptions) (RetrievedContext, error) {
	if strings.TrimSpace(userID) == "" {
		return RetrievedContext{}, nil
	}
	if opts.RecentTurns <= 0 {
		opts.RecentTurns = 4
	}
	if opts.ContextBudgetChars <= 0 {
		opts.ContextBudgetChars = 12000
	}

	recent, err := s.RecentSessionTurns(sessionID, 1, opts.RecentTurns)
	if err != nil {
		return RetrievedContext{}, err
	}

	block := s.renderContextBlock(recent, opts.ContextBudgetChars)
	return RetrievedContext{Block: block, RecentTurnCount: len(recent)}, nil
}

func (s *Store) renderContextBlock(recent []SessionTurn, maxChars int) string {
	var b strings.Builder
	b.WriteString("# Retrieved Memory\n")
	if len(recent) > 0 {
		b.WriteString("\n## Recent Exchanges\n")
		writeTurns(&b, recent)
	}
	text := strings.TrimSpace(b.String())
	if text == "# Retrieved Memory" {
		return ""
	}
	if len(text) > maxChars {
		text = text[:maxChars] + "..."
	}
	return text
}

func writeEntries(b *strings.Builder, entries []MemoryEntry) {
	for _, entry := range entries {
		fmt.Fprintf(b, "- [%s/%s, importance %d] %s\n", entry.Scope, entry.Category, entry.Importance, strings.TrimSpace(entry.Statement))
		if strings.TrimSpace(entry.Evidence) != "" {
			fmt.Fprintf(b, "  Evidence: %s\n", strings.TrimSpace(entry.Evidence))
		}
	}
}

func writeTurns(b *strings.Builder, turns []SessionTurn) {
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		fmt.Fprintf(b, "User: %s\nAssistant: %s\n\n", strings.TrimSpace(turn.UserText), strings.TrimSpace(turn.AssistantText))
	}
}

func (s *Store) activeEntries(userID, scope, category string) ([]MemoryEntry, error) {
	query := `SELECT id, canonical_user_id, scope, category, statement, evidence, confidence, importance, status, source_session_id, created_at, updated_at, last_used_at, expires_at, COALESCE(supersedes_id, 0), embedding_model, embedding_dim FROM memory_entries WHERE canonical_user_id = ? AND status = 'active'`
	args := []any{userID}
	if scope != "" {
		query += ` AND scope = ?`
		args = append(args, scope)
	}
	if category != "" {
		query += ` AND category = ?`
		args = append(args, category)
	}
	query += ` ORDER BY importance DESC, updated_at DESC`
	rows, err := s.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to read memories: %w", err)
	}
	defer rows.Close()
	entries := []MemoryEntry{}
	for rows.Next() {
		entry, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func scanMemoryEntry(rows interface{ Scan(...any) error }) (MemoryEntry, error) {
	var entry MemoryEntry
	var created, updated string
	var lastUsed, expires sql.NullString
	if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Scope, &entry.Category, &entry.Statement, &entry.Evidence, &entry.Confidence, &entry.Importance, &entry.Status, &entry.SourceSessionID, &created, &updated, &lastUsed, &expires, &entry.SupersedesID, &entry.EmbeddingModel, &entry.EmbeddingDim); err != nil {
		return MemoryEntry{}, fmt.Errorf("failed to scan memory entry: %w", err)
	}
	entry.CreatedAt = parseTime(created)
	entry.UpdatedAt = parseTime(updated)
	if lastUsed.Valid {
		entry.LastUsedAt = parseTime(lastUsed.String)
	}
	if expires.Valid {
		entry.ExpiresAt = parseTime(expires.String)
	}
	return entry, nil
}

func scanSessionTurn(rows interface{ Scan(...any) error }) (SessionTurn, error) {
	var turn SessionTurn
	var toolNames, topicTags, created string
	var expires sql.NullString
	if err := rows.Scan(&turn.ID, &turn.SessionID, &turn.UserID, &turn.UserText, &turn.AssistantText, &toolNames, &turn.Importance, &topicTags, &created, &expires); err != nil {
		return SessionTurn{}, fmt.Errorf("failed to scan session turn: %w", err)
	}
	turn.ToolNames = splitCSV(toolNames)
	turn.TopicTags = splitCSV(topicTags)
	turn.CreatedAt = parseTime(created)
	if expires.Valid {
		turn.ExpiresAt = parseTime(expires.String)
	}
	return turn, nil
}

func (s *Store) storeMemoryVector(rowID int64, embedding []float64) error {
	if rowID <= 0 || len(embedding) == 0 {
		return nil
	}
	if err := s.ensureVectorTable("memory_entry_vectors", len(embedding)); err != nil {
		return err
	}
	serialized, err := serializeVector(embedding)
	if err != nil {
		return err
	}
	if _, err := s.sql.Exec(`INSERT OR REPLACE INTO memory_entry_vectors(rowid, embedding) VALUES (?, ?)`, rowID, serialized); err != nil {
		return fmt.Errorf("failed to store memory vector: %w", err)
	}
	return nil
}

func (s *Store) searchMemoryVectors(queryVector []float64, userID, scope, category string, limit int) ([]MemoryEntry, bool, error) {
	if len(queryVector) == 0 || !s.vectorTableExists("memory_entry_vectors") {
		return nil, false, nil
	}
	serialized, err := serializeVector(queryVector)
	if err != nil {
		return nil, false, err
	}
	k := limit * 5
	if k < limit {
		k = limit
	}
	if k < 25 {
		k = 25
	}
	query := `
SELECT e.id, e.canonical_user_id, e.scope, e.category, e.statement, e.evidence, e.confidence, e.importance, e.status, e.source_session_id, e.created_at, e.updated_at, e.last_used_at, e.expires_at, COALESCE(e.supersedes_id, 0), e.embedding_model, e.embedding_dim, v.distance
FROM memory_entry_vectors v
JOIN memory_entries e ON e.id = v.rowid
WHERE v.embedding MATCH ? AND v.k = ? AND e.canonical_user_id = ? AND e.status = 'active'`
	args := []any{serialized, k, userID}
	if scope != "" {
		query += ` AND e.scope = ?`
		args = append(args, scope)
	}
	if category != "" {
		query += ` AND e.category = ?`
		args = append(args, category)
	}
	query += ` ORDER BY v.distance LIMIT ?`
	args = append(args, limit)
	rows, err := s.sql.Query(query, args...)
	if err != nil {
		return nil, true, fmt.Errorf("failed to search memory vectors: %w", err)
	}
	defer rows.Close()
	entries := []MemoryEntry{}
	for rows.Next() {
		entry, distance, err := scanMemoryEntryWithDistance(rows)
		if err != nil {
			return nil, true, err
		}
		entry.Score = vectorHybridScore(distance, entry.Importance, entry.UpdatedAt)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, true, err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
	return entries, true, nil
}

func scanMemoryEntryWithDistance(rows interface{ Scan(...any) error }) (MemoryEntry, float64, error) {
	var entry MemoryEntry
	var created, updated string
	var lastUsed, expires sql.NullString
	var distance float64
	if err := rows.Scan(&entry.ID, &entry.UserID, &entry.Scope, &entry.Category, &entry.Statement, &entry.Evidence, &entry.Confidence, &entry.Importance, &entry.Status, &entry.SourceSessionID, &created, &updated, &lastUsed, &expires, &entry.SupersedesID, &entry.EmbeddingModel, &entry.EmbeddingDim, &distance); err != nil {
		return MemoryEntry{}, 0, fmt.Errorf("failed to scan memory vector result: %w", err)
	}
	entry.CreatedAt = parseTime(created)
	entry.UpdatedAt = parseTime(updated)
	if lastUsed.Valid {
		entry.LastUsedAt = parseTime(lastUsed.String)
	}
	if expires.Valid {
		entry.ExpiresAt = parseTime(expires.String)
	}
	return entry, distance, nil
}

func (s *Store) ensureVectorTable(name string, dimension int) error {
	if dimension <= 0 {
		return fmt.Errorf("embedding dimension must be positive")
	}
	if dim, ok := s.vectorTableDimension(name); ok && dim == dimension {
		return nil
	}
	if _, err := s.sql.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
		return fmt.Errorf("failed to drop stale vector table %s: %w", name, err)
	}
	if _, err := s.sql.Exec(fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING vec0(embedding float[%d])`, name, dimension)); err != nil {
		return fmt.Errorf("failed to create vector table %s: %w", name, err)
	}
	return nil
}

func (s *Store) vectorTableExists(name string) bool {
	_, ok := s.vectorTableDimension(name)
	return ok
}

func (s *Store) vectorTableDimension(name string) (int, bool) {
	var sqlText string
	err := s.sql.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&sqlText)
	if err != nil || !strings.Contains(sqlText, "float[") {
		return 0, false
	}
	start := strings.Index(sqlText, "float[") + len("float[")
	end := strings.Index(sqlText[start:], "]")
	if end < 0 {
		return 0, false
	}
	var dim int
	if _, err := fmt.Sscanf(sqlText[start:start+end], "%d", &dim); err != nil || dim <= 0 {
		return 0, false
	}
	return dim, true
}

func serializeVector(values []float64) ([]byte, error) {
	vector := make([]float32, 0, len(values))
	for _, value := range values {
		vector = append(vector, float32(value))
	}
	serialized, err := sqlite_vec.SerializeFloat32(vector)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize embedding vector: %w", err)
	}
	return serialized, nil
}

func distanceToSimilarity(distance float64) float64 {
	if distance < 0 {
		distance = 0
	}
	return 1 / (1 + distance)
}

func vectorHybridScore(distance float64, importance int, updatedAt time.Time) float64 {
	return distanceToSimilarity(distance)*0.55 + (float64(importance)/5)*0.20 + recencyScore(updatedAt)*0.15 + 0.10
}

func (s *Store) markSuperseded(userID, scope, statement string) (int64, error) {
	var id int64
	err := s.sql.QueryRow(`SELECT id FROM memory_entries WHERE canonical_user_id = ? AND scope = ? AND statement_key = ? AND status = 'active'`, userID, normalizeScope(scope), statementKey(statement)).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to find superseded memory: %w", err)
	}
	if _, err := s.sql.Exec(`UPDATE memory_entries SET status = 'superseded', updated_at = ? WHERE id = ?`, formatTime(time.Now()), id); err != nil {
		return 0, fmt.Errorf("failed to supersede memory: %w", err)
	}
	return id, nil
}

func (s *Store) expireOldMemories() error {
	_, err := s.sql.Exec(`UPDATE memory_entries SET status = 'expired', updated_at = ? WHERE status = 'active' AND expires_at IS NOT NULL AND datetime(expires_at) <= datetime(?)`, formatTime(time.Now()), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("failed to expire memories: %w", err)
	}
	return nil
}

func (s *Store) expireOldSessionTurns() error {
	_, err := s.sql.Exec(`DELETE FROM session_turns WHERE expires_at IS NOT NULL AND datetime(expires_at) <= datetime(?)`, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("failed to expire session turns: %w", err)
	}
	return nil
}

func (s *Store) recordEvent(memoryID int64, eventType, requestID, sessionID, metadata string) {
	_, _ = s.sql.Exec(`INSERT INTO memory_events (memory_id, event_type, request_id, session_id, created_at, metadata) VALUES (?, ?, ?, ?, ?, ?)`, nullableID(memoryID), eventType, requestID, sessionID, formatTime(time.Now()), metadata)
}

func (s *Store) ensureAccountUser(userID string) error {
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("user memory: user id is required")
	}
	now := formatTime(time.Now())
	_, err := s.sql.Exec(`INSERT OR IGNORE INTO account_users (canonical_user_id, created_at, updated_at) VALUES (?, ?, ?)`, userID, now, now)
	if err != nil {
		return fmt.Errorf("failed to ensure account user %q: %w", userID, err)
	}
	return nil
}

func (s *Store) embedBestEffort(ctx context.Context, text string) []float64 {
	if s == nil || s.embedder == nil || strings.TrimSpace(s.embedModel) == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	resp, err := s.embedder.Embed(ctx, llm.EmbedRequest{Model: s.embedModel, Input: strings.TrimSpace(text)})
	if err != nil || len(resp.Embeddings) == 0 {
		return nil
	}
	return append([]float64(nil), resp.Embeddings[0]...)
}

func memoryEmbeddingText(scope, category, statement, evidence string) string {
	return strings.TrimSpace(scope + "\n" + category + "\n" + statement + "\nEvidence: " + evidence)
}

func normalizeScope(scope string) string {
	scope = strings.TrimSpace(strings.ToLower(scope))
	scope = strings.ReplaceAll(scope, "-", "_")
	scope = strings.ReplaceAll(scope, " ", "_")
	if scope == ScopeLongTerm || scope == "long" || scope == "persistent" {
		return ScopeLongTerm
	}
	return ScopeShortTerm
}

func normalizeOptionalScope(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return ""
	}
	return normalizeScope(scope)
}

func normalizeCategory(cat string) string {
	cat = strings.TrimSpace(strings.ToLower(cat))
	cat = strings.ReplaceAll(cat, "-", "_")
	cat = strings.ReplaceAll(cat, " ", "_")
	if cat == "preferences" {
		cat = "durable_preferences"
	}
	for _, valid := range ValidCategories {
		if cat == valid {
			return cat
		}
	}
	return "notes"
}

func normalizeOptionalCategory(cat string) string {
	if strings.TrimSpace(cat) == "" {
		return ""
	}
	return normalizeCategory(cat)
}

func statementKey(statement string) string {
	return strings.ToLower(strings.Join(strings.Fields(statement), " "))
}

func clampInt(value, minValue, maxValue, fallback int) int {
	if value == 0 {
		return fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return t
}

func recencyScore(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	age := time.Since(t)
	if age <= 0 {
		return 1
	}
	return 1 / (1 + age.Hours()/168)
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	lines := strings.Split(value, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(strings.TrimPrefix(line, "-")); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// RenderMemory formats entries as compact Markdown for tools and stream payloads.
func RenderMemory(intro string, entries []MemoryEntry) string {
	var b strings.Builder
	if strings.TrimSpace(intro) != "" {
		b.WriteString(strings.TrimSpace(intro))
		b.WriteString("\n\n")
	}
	if len(entries) == 0 {
		return strings.TrimSpace(b.String())
	}
	b.WriteString("# User Memory\n")
	byHeading := map[string][]MemoryEntry{}
	for _, entry := range entries {
		heading := displayCategoryName(entry.Scope + " " + entry.Category)
		byHeading[heading] = append(byHeading[heading], entry)
	}
	headings := make([]string, 0, len(byHeading))
	for heading := range byHeading {
		headings = append(headings, heading)
	}
	sort.Strings(headings)
	for _, heading := range headings {
		b.WriteString("\n## ")
		b.WriteString(heading)
		b.WriteString("\n\n")
		for _, entry := range byHeading[heading] {
			b.WriteString(strings.TrimSpace(entry.Statement))
			b.WriteString("\n\n- Evidence: ")
			b.WriteString(strings.TrimSpace(entry.Evidence))
			b.WriteString(".\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func displayCategoryName(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	parts := strings.Fields(value)
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

// ParsedContent is a UI-friendly representation of rendered memory content.
type ParsedContent struct {
	Intro    string            `json:"intro,omitempty"`
	Sections map[string]string `json:"sections,omitempty"`
}

// ParseContent converts rendered memory Markdown into sections for streaming UIs.
func ParseContent(content string) ParsedContent {
	parsed := ParsedContent{Sections: map[string]string{}}
	content = strings.TrimSpace(content)
	if content == "" {
		return parsed
	}
	lines := strings.Split(content, "\n")
	var current string
	var b strings.Builder
	flush := func() {
		if current != "" {
			parsed.Sections[current] = strings.TrimSpace(b.String())
			b.Reset()
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "# User Memory") {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			flush()
			current = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		if current == "" && strings.HasPrefix(strings.TrimSpace(line), "You are speaking with ") {
			parsed.Intro = strings.TrimSpace(line)
			continue
		}
		if current != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	if len(parsed.Sections) == 0 {
		parsed.Sections = nil
	}
	return parsed
}
