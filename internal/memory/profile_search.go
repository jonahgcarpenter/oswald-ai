package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SearchHit identifies one delivered message matched by a session search.
type SearchHit struct {
	SessionID string
	MessageID int64
	Role      string
	Content   string
	Timestamp time.Time
	Snippet   string
	Rank      float64
}

// SearchSession is one deduplicated session lineage returned by discovery.
type SearchSession struct {
	SessionID      string
	Source         string
	Model          string
	Title          string
	StartedAt      time.Time
	LastActive     time.Time
	MessageCount   int
	MatchMessageID int64
	MatchedRole    string
	Snippet        string
}

// SearchMessage is one delivered message rendered in a window or transcript.
type SearchMessage struct {
	ID        int64
	Role      string
	Content   string
	ToolName  string
	ToolCalls string
	Timestamp time.Time
}

// SessionSummaryRecord describes one delivered session for browse results.
type SessionSummaryRecord struct {
	SessionID    string
	Source       string
	Model        string
	Title        string
	StartedAt    time.Time
	LastActive   time.Time
	MessageCount int
	Preview      string
}

const (
	// searchEligible is the delivery and profile filter shared by every shape.
	// Generation equality is intentionally not required: /new keeps prior
	// sessions as searchable history, and nothing eager-deletes delivered rows.
	searchEligible = `m.active=1 AND s.profile_name=?`
)

// SearchFilter bounds a discovery query.
type SearchFilter struct {
	Query   string
	Limit   int
	Sort    string
	After   *time.Time
	Before  *time.Time
	Exclude []string
	// LiveSessionID is the active conversation's session id; it and its lineage
	// are omitted because that content is already in the model's live context.
	LiveSessionID string
}

// DiscoverySessions runs an FTS5 search over delivered messages and returns one
// deduplicated session per lineage. It performs no LLM work and returns stored
// message rows only.
func (s *ProfileStore) DiscoverySessions(ctx context.Context, owner string, filter SearchFilter) ([]SearchSession, error) {
	if owner != s.profile {
		return nil, errors.New("invalid profile search scope")
	}
	if strings.TrimSpace(filter.Query) == "" {
		return nil, errors.New("search query is required")
	}
	if filter.Limit <= 0 || filter.Limit > 40 {
		filter.Limit = 8
	}
	excluded, err := s.excludedLineage(ctx, owner, filter.Exclude)
	if err != nil {
		return nil, err
	}
	if err := s.addLiveLineage(ctx, owner, filter.LiveSessionID, excluded); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
SELECT m.session_id, m.id, m.role, COALESCE(m.content,''), m.timestamp,
       snippet(messages_fts, 0, '[', ']', '…', 24) AS snip
FROM messages_fts
JOIN messages m ON m.id = messages_fts.rowid
JOIN sessions s ON s.id = m.session_id
WHERE messages_fts MATCH ? AND `+searchEligible+`
ORDER BY bm25(messages_fts) LIMIT ?`, filter.Query, owner, filter.Limit*4)
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	for rows.Next() {
		var hit SearchHit
		var timestamp float64
		if err := rows.Scan(&hit.SessionID, &hit.MessageID, &hit.Role, &hit.Content, &timestamp, &hit.Snippet); err != nil {
			rows.Close()
			return nil, err
		}
		hit.Timestamp = secondsToTime(timestamp)
		hits = append(hits, hit)
	}
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return nil, iterationErr
	}
	return s.rankLineages(ctx, owner, hits, filter, excluded)
}

// rankLineages resolves each hit to its lineage root, keeps the strongest hit
// per lineage, applies temporal bounds and exclusions, and hydrates metadata.
func (s *ProfileStore) rankLineages(ctx context.Context, owner string, hits []SearchHit, filter SearchFilter, excluded map[string]bool) ([]SearchSession, error) {
	type lineageHit struct {
		hit     SearchHit
		sortKey time.Time
	}
	best := map[string]lineageHit{}
	order := make([]string, 0, len(hits))
	for _, hit := range hits {
		root, err := s.lineageRoot(ctx, owner, hit.SessionID)
		if err != nil {
			return nil, err
		}
		if root == "" || excluded[root] {
			continue
		}
		if filter.After != nil && hit.Timestamp.Before(*filter.After) {
			continue
		}
		if filter.Before != nil && !hit.Timestamp.Before(*filter.Before) {
			continue
		}
		current, ok := best[root]
		if !ok {
			order = append(order, root)
			best[root] = lineageHit{hit: hit, sortKey: hit.Timestamp}
			continue
		}
		if filter.Sort == "newest" && hit.Timestamp.After(current.sortKey) {
			best[root] = lineageHit{hit: hit, sortKey: hit.Timestamp}
		} else if filter.Sort == "oldest" && hit.Timestamp.Before(current.sortKey) {
			best[root] = lineageHit{hit: hit, sortKey: hit.Timestamp}
		}
	}
	if filter.Sort == "newest" || filter.Sort == "oldest" {
		descending := filter.Sort == "newest"
		for i := 1; i < len(order); i++ {
			for j := i; j > 0; j-- {
				left, right := best[order[j-1]].sortKey, best[order[j]].sortKey
				swap := (descending && right.After(left)) || (!descending && right.Before(left))
				if !swap {
					break
				}
				order[j-1], order[j] = order[j], order[j-1]
			}
		}
	}
	if len(order) > filter.Limit {
		order = order[:filter.Limit]
	}
	results := make([]SearchSession, 0, len(order))
	for _, root := range order {
		entry := best[root]
		summary, err := s.sessionSummary(ctx, owner, root)
		if err != nil {
			return nil, err
		}
		if summary == nil {
			continue
		}
		summary.MatchMessageID = entry.hit.MessageID
		summary.MatchedRole = entry.hit.Role
		summary.Snippet = entry.hit.Snippet
		results = append(results, *summary)
	}
	return results, nil
}

// addLiveLineage marks the live conversation's lineage root as excluded so
// already-in-context content is never recalled.
func (s *ProfileStore) addLiveLineage(ctx context.Context, owner, liveSessionID string, excluded map[string]bool) error {
	if strings.TrimSpace(liveSessionID) == "" {
		return nil
	}
	root, err := s.lineageRoot(ctx, owner, liveSessionID)
	if err != nil {
		return err
	}
	if root != "" {
		excluded[root] = true
	}
	return nil
}

// excludedLineage expands inspected session ids into their full lineage roots.
func (s *ProfileStore) excludedLineage(ctx context.Context, owner string, ids []string) (map[string]bool, error) {
	excluded := map[string]bool{}
	if len(ids) == 0 {
		return excluded, nil
	}
	if len(ids) > 20 {
		ids = ids[:20]
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		root, err := s.lineageRoot(ctx, owner, id)
		if err != nil {
			return nil, err
		}
		if root != "" {
			excluded[root] = true
		}
	}
	return excluded, nil
}

// lineageRoot walks parents to the oldest related session delivered to owner.
// The supplied identifier may be a session id or a session key; the latter is
// accepted so an `@session` link echoed from older context still resolves.
func (s *ProfileStore) lineageRoot(ctx context.Context, owner, id string) (string, error) {
	current := id
	for depth := 0; depth < 16; depth++ {
		var parent sql.NullString
		err := s.db.SQL().QueryRowContext(ctx, `SELECT parent_session_id FROM sessions WHERE id=? AND profile_name=?`, current, owner).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			// Fall back to the newest session whose session key matches.
			if err := s.db.SQL().QueryRowContext(ctx, `SELECT id FROM sessions WHERE session_key=? AND profile_name=? ORDER BY started_at DESC LIMIT 1`, id, owner).Scan(&current); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return "", nil
				}
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if !parent.Valid || parent.String == "" || parent.String == current {
			return current, nil
		}
		current = parent.String
	}
	return current, nil
}

func (s *ProfileStore) sessionSummary(ctx context.Context, owner, id string) (*SearchSession, error) {
	var summary SearchSession
	var started, last sql.NullFloat64
	var title, model sql.NullString
	err := s.db.SQL().QueryRowContext(ctx, `SELECT id, source, COALESCE(model,''), COALESCE(title,''), started_at, COALESCE(last_activity_at, started_at), COALESCE(message_count,0) FROM sessions WHERE id=? AND profile_name=?`, id, owner).Scan(&summary.SessionID, &summary.Source, &model, &title, &started, &last, &summary.MessageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	summary.Model = model.String
	summary.Title = title.String
	summary.StartedAt = secondsToTime(started.Float64)
	summary.LastActive = secondsToTime(last.Float64)
	return &summary, nil
}

// SessionWindow returns an anchored slice of delivered messages around anchorID.
// window is clamped to [1,20]. Delivery and generation fencing always apply.
func (s *ProfileStore) SessionWindow(ctx context.Context, owner, sessionID string, anchorID int64, window int) (before []SearchMessage, anchor *SearchMessage, after []SearchMessage, err error) {
	if owner != s.profile {
		return nil, nil, nil, errors.New("invalid profile search scope")
	}
	if window < 1 {
		window = 1
	}
	if window > 20 {
		window = 20
	}
	if anchorID <= 0 {
		return nil, nil, nil, errors.New("anchor message is required")
	}
	root, err := s.lineageRoot(ctx, owner, sessionID)
	if err != nil || root == "" {
		return nil, nil, nil, errors.New("session not found in this profile")
	}
	anchor, err = s.messageByID(ctx, owner, anchorID)
	if err != nil || anchor == nil {
		return nil, nil, nil, errors.New("anchor message not found in this profile")
	}
	before, err = s.windowMessages(ctx, owner, anchorID, window, false)
	if err != nil {
		return nil, nil, nil, err
	}
	after, err = s.windowMessages(ctx, owner, anchorID, window, true)
	if err != nil {
		return nil, nil, nil, err
	}
	return before, anchor, after, nil
}

func (s *ProfileStore) messageByID(ctx context.Context, owner string, id int64) (*SearchMessage, error) {
	row := s.db.SQL().QueryRowContext(ctx, `SELECT m.id, m.role, COALESCE(m.content,''), COALESCE(m.tool_name,''), COALESCE(m.tool_calls,''), m.timestamp FROM messages m JOIN sessions s ON s.id=m.session_id WHERE m.id=? AND `+searchEligible, id, owner)
	message, err := scanSearchMessage(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &message, nil
}

func (s *ProfileStore) windowMessages(ctx context.Context, owner string, anchorID int64, limit int, forward bool) ([]SearchMessage, error) {
	comparison, order := "<", "DESC"
	if forward {
		comparison, order = ">", "ASC"
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT m.id, m.role, COALESCE(m.content,''), COALESCE(m.tool_name,''), COALESCE(m.tool_calls,''), m.timestamp FROM messages m JOIN sessions s ON s.id=m.session_id WHERE m.id `+comparison+` ? AND `+searchEligible+` ORDER BY m.id `+order+` LIMIT ?`, anchorID, owner, limit)
	if err != nil {
		return nil, err
	}
	var messages []SearchMessage
	for rows.Next() {
		message, err := scanSearchMessage(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		messages = append(messages, message)
	}
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return nil, iterationErr
	}
	if !forward {
		for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
			messages[left], messages[right] = messages[right], messages[left]
		}
	}
	return messages, nil
}

// SessionTranscript returns the transcript for one delivered session, bounded
// to first and last rows when the session is larger than the chunk.
func (s *ProfileStore) SessionTranscript(ctx context.Context, owner, sessionID string, head, tail int) (first, last []SearchMessage, count int, err error) {
	if owner != s.profile {
		return nil, nil, 0, errors.New("invalid profile search scope")
	}
	root, err := s.lineageRoot(ctx, owner, sessionID)
	if err != nil || root == "" {
		return nil, nil, 0, errors.New("session not found in this profile")
	}
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COALESCE(message_count,0) FROM sessions WHERE id=? AND profile_name=?`, root, owner).Scan(&count); err != nil {
		return nil, nil, 0, err
	}
	first, err = s.transcriptChunk(ctx, owner, root, head, false)
	if err != nil {
		return nil, nil, 0, err
	}
	if count > head+tail {
		last, err = s.transcriptChunk(ctx, owner, root, tail, true)
		if err != nil {
			return nil, nil, 0, err
		}
	}
	return first, last, count, nil
}

func (s *ProfileStore) transcriptChunk(ctx context.Context, owner, sessionID string, limit int, descending bool) ([]SearchMessage, error) {
	order := "ASC"
	if descending {
		order = "DESC"
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT m.id, m.role, COALESCE(m.content,''), COALESCE(m.tool_name,''), COALESCE(m.tool_calls,''), m.timestamp FROM messages m JOIN sessions s ON s.id=m.session_id WHERE m.session_id=? AND `+searchEligible+` ORDER BY m.id `+order+` LIMIT ?`, sessionID, owner, limit)
	if err != nil {
		return nil, err
	}
	var messages []SearchMessage
	for rows.Next() {
		message, err := scanSearchMessage(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, err
		}
		messages = append(messages, message)
	}
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return nil, iterationErr
	}
	if descending {
		for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
			messages[left], messages[right] = messages[right], messages[left]
		}
	}
	return messages, nil
}

// RecentSessions returns the most recently active delivered sessions.
func (s *ProfileStore) RecentSessions(ctx context.Context, owner string, limit int) ([]SessionSummaryRecord, error) {
	if owner != s.profile {
		return nil, errors.New("invalid profile search scope")
	}
	if limit <= 0 || limit > 40 {
		limit = 8
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
SELECT s.id, s.source, COALESCE(s.model,''), COALESCE(s.title,''),
       s.started_at, COALESCE(s.last_activity_at, s.started_at), COALESCE(s.message_count,0),
       COALESCE((SELECT m.content FROM messages m WHERE m.session_id=s.id AND m.active=1 ORDER BY m.id LIMIT 1),'')
FROM sessions s
WHERE s.profile_name=?
ORDER BY COALESCE(s.last_activity_at, s.started_at) DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	var records []SessionSummaryRecord
	for rows.Next() {
		var record SessionSummaryRecord
		var started, last float64
		if err := rows.Scan(&record.SessionID, &record.Source, &record.Model, &record.Title, &started, &last, &record.MessageCount, &record.Preview); err != nil {
			rows.Close()
			return nil, err
		}
		record.StartedAt = secondsToTime(started)
		record.LastActive = secondsToTime(last)
		records = append(records, record)
	}
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return nil, iterationErr
	}
	return records, nil
}

type scanFunc func(dest ...any) error

func scanSearchMessage(scan scanFunc) (SearchMessage, error) {
	var message SearchMessage
	var timestamp float64
	if err := scan(&message.ID, &message.Role, &message.Content, &message.ToolName, &message.ToolCalls, &timestamp); err != nil {
		return message, err
	}
	message.Timestamp = secondsToTime(timestamp)
	return message, nil
}

func secondsToTime(seconds float64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(seconds*1e9)).UTC()
}

// Profile returns the trusted profile this store serves.
func (s *ProfileStore) Profile() string { return s.profile }

// SessionSummaryFor returns delivered session metadata for one session id.
func (s *ProfileStore) SessionSummaryFor(ctx context.Context, owner, id string) (*SearchSession, error) {
	if owner != s.profile {
		return nil, errors.New("invalid profile search scope")
	}
	root, err := s.lineageRoot(ctx, owner, id)
	if err != nil || root == "" {
		return nil, errors.New("session not found in this profile")
	}
	return s.sessionSummary(ctx, owner, root)
}

// ActiveSessionID returns the current open session id for a conversation and
// generation, or "" when none is active. It never advances generation.
func (s *ProfileStore) ActiveSessionID(ctx context.Context, owner, key string, generation int) (string, error) {
	if owner != s.profile {
		return "", errors.New("invalid profile search scope")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// SharesLineage reports whether two session ids resolve to the same lineage.
func (s *ProfileStore) SharesLineage(ctx context.Context, owner, left, right string) (bool, error) {
	if owner != s.profile {
		return false, errors.New("invalid profile search scope")
	}
	leftRoot, err := s.lineageRoot(ctx, owner, left)
	if err != nil || leftRoot == "" {
		return false, err
	}
	rightRoot, err := s.lineageRoot(ctx, owner, right)
	if err != nil || rightRoot == "" {
		return false, err
	}
	return leftRoot == rightRoot, nil
}

// RecentSessionsExcluding returns recent delivered sessions with the live
// conversation's lineage omitted.
func (s *ProfileStore) RecentSessionsExcluding(ctx context.Context, owner, liveSessionID string, limit int) ([]SessionSummaryRecord, error) {
	records, err := s.RecentSessions(ctx, owner, limit+1)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(liveSessionID) == "" {
		if len(records) > limit {
			records = records[:limit]
		}
		return records, nil
	}
	liveRoot, err := s.lineageRoot(ctx, owner, liveSessionID)
	if err != nil {
		return nil, err
	}
	filtered := records[:0]
	for _, record := range records {
		root, err := s.lineageRoot(ctx, owner, record.SessionID)
		if err != nil {
			return nil, err
		}
		if root != "" && root == liveRoot {
			continue
		}
		filtered = append(filtered, record)
		if len(filtered) == limit {
			break
		}
	}
	return filtered, nil
}
