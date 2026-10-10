package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

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
	ID         int64
	Role       string
	Content    string
	ToolName   string
	ToolCalls  string
	ToolCallID string
	History    ToolHistory
	Timestamp  time.Time
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
	searchEligible       = `m.active=1 AND s.profile_name=?`
	searchMessageColumns = `m.id,m.role,COALESCE(m.content,''),COALESCE(m.tool_name,''),COALESCE(m.tool_calls,''),COALESCE(m.tool_call_id,''),m.timestamp,COALESCE(v.value,'')`
	searchMessageJoin    = ` FROM messages m JOIN sessions s ON s.id=m.session_id LEFT JOIN state_meta v ON m.role='assistant' AND v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) `
)

// SearchFilter bounds a discovery query.
type SearchFilter struct {
	Query   string
	Limit   int
	Sort    string
	After   *time.Time
	Before  *time.Time
	Exclude []string
	// Roles controls matching, not surrounding context. Nil defaults to user and assistant.
	Roles map[string]bool
	// LiveSessionID is the active conversation's session id; it and the
	// sessions forked from it are omitted. Older lineage generations predate
	// the live conversation and remain recallable, deduped to one entry per
	// lineage root.
	LiveSessionID string
	// GroupSource and GroupID constrain discovery to one group conversation
	// (for example one Discord channel or iMessage chat). Both must be set
	// together; when empty, no group constraint applies.
	GroupSource string
	GroupID     string
}

// ParseGroupSessionKey splits a group conversation session key of the form
// source:group:sender (for example discord:<channel>:<author> or
// imessage:<chatGUID>:<sender>) and reports the source and group segments.
// DM keys (source:dm:sender) and malformed keys report ok=false.
func ParseGroupSessionKey(key string) (source, groupID string, ok bool) {
	first := strings.Index(key, ":")
	last := strings.LastIndex(key, ":")
	if first < 0 || last <= first {
		return "", "", false
	}
	source, groupID, sender := key[:first], key[first+1:last], key[last+1:]
	if source != "discord" && source != "imessage" {
		return "", "", false
	}
	if groupID == "" || groupID == "dm" || sender == "" {
		return "", "", false
	}
	if strings.Contains(groupID, ":") || strings.Contains(sender, ":") {
		// Group and sender segments must not contain further separators;
		// first/last split above already isolates them, so any inner colon
		// means a malformed key rather than a nested namespace.
		if strings.Contains(groupID, ":") {
			return "", "", false
		}
	}
	return source, groupID, true
}

// SessionKeyFor returns the conversation key and source for one session id
// (or conversation key, resolved to its newest session). It reports
// sql.ErrNoRows when the session does not exist in this profile.
func (s *ProfileStore) SessionKeyFor(ctx context.Context, owner, id string) (sessionKey, source string, err error) {
	if owner != s.profile {
		return "", "", errors.New("invalid profile search scope")
	}
	resolved, err := s.resolveSearchSession(ctx, owner, id)
	if err != nil {
		return "", "", err
	}
	if resolved == "" {
		return "", "", sql.ErrNoRows
	}
	err = s.db.SQL().QueryRowContext(ctx, `SELECT session_key,source FROM sessions WHERE id=? AND profile_name=?`, resolved, owner).Scan(&sessionKey, &source)
	if err != nil {
		return "", "", err
	}
	return sessionKey, source, nil
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
	return s.discoverIndexed(ctx, owner, filter)
}

// lineageSubtree returns the live session plus every session descended from
// it. Older generations predate the live conversation and stay visible;
// forks extend it and are omitted with it. The identifier may be a session
// id or key; unknown identifiers exclude nothing.
func (s *ProfileStore) lineageSubtree(ctx context.Context, owner, live string) (map[string]bool, error) {
	subtree := map[string]bool{}
	if strings.TrimSpace(live) == "" {
		return subtree, nil
	}
	id, err := s.resolveSearchSession(ctx, owner, live)
	if err != nil || id == "" {
		return subtree, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT id,COALESCE(parent_session_id,'') FROM sessions WHERE profile_name=?`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := make(map[string][]string)
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			return nil, err
		}
		children[parent] = append(children[parent], child)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	queue := []string{id}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if subtree[current] {
			continue
		}
		subtree[current] = true
		queue = append(queue, children[current]...)
	}
	return subtree, nil
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
	err := s.db.SQL().QueryRowContext(ctx, `SELECT s.id,s.source,COALESCE(s.model,''),COALESCE(s.title,''),s.started_at,COALESCE(s.last_activity_at,s.started_at),(SELECT COUNT(*) FROM messages m WHERE m.session_id=s.id AND m.active=1) FROM sessions s WHERE s.id=? AND s.profile_name=?`, id, owner).Scan(&summary.SessionID, &summary.Source, &model, &title, &started, &last, &summary.MessageCount)
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
// window is clamped to [1,20]. Only delivered rows from this session apply.
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
	id, err := s.resolveSearchSession(ctx, owner, sessionID)
	if err != nil {
		return nil, nil, nil, err
	}
	if id == "" {
		return nil, nil, nil, errors.New("session not found in this profile")
	}
	anchor, err = s.messageByID(ctx, owner, id, anchorID)
	if err != nil {
		return nil, nil, nil, err
	}
	if anchor == nil {
		return nil, nil, nil, errors.New("anchor message not found in this profile")
	}
	before, err = s.windowMessages(ctx, owner, id, anchorID, window, false)
	if err != nil {
		return nil, nil, nil, err
	}
	after, err = s.windowMessages(ctx, owner, id, anchorID, window, true)
	if err != nil {
		return nil, nil, nil, err
	}
	return before, anchor, after, nil
}

// SessionWindowRemainder counts delivered messages outside a returned window.
// Boundary ids must be from the requested session, just like scroll anchors.
func (s *ProfileStore) SessionWindowRemainder(ctx context.Context, owner, sessionID string, firstID, lastID int64) (before, after int, err error) {
	if owner != s.profile || firstID <= 0 || lastID < firstID {
		return 0, 0, errors.New("invalid profile search window")
	}
	id, err := s.resolveSearchSession(ctx, owner, sessionID)
	if err != nil {
		return 0, 0, err
	}
	if id == "" {
		return 0, 0, errors.New("session not found in this profile")
	}
	var boundaries int
	err = s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(m.id<?),0),COALESCE(SUM(m.id>?),0) FROM messages m JOIN sessions s ON s.id=m.session_id WHERE m.session_id=? AND `+searchEligible+` AND (m.id IN (?,?) OR m.id<? OR m.id>?)`, firstID, lastID, id, owner, firstID, lastID, firstID, lastID).Scan(&boundaries, &before, &after)
	if err != nil {
		return 0, 0, err
	}
	want := 2
	if firstID == lastID {
		want = 1
	}
	if boundaries-before-after != want {
		return 0, 0, errors.New("window boundaries not found in session")
	}
	return before, after, nil
}

func (s *ProfileStore) messageByID(ctx context.Context, owner, sessionID string, id int64) (*SearchMessage, error) {
	row := s.db.SQL().QueryRowContext(ctx, `SELECT `+searchMessageColumns+searchMessageJoin+`WHERE m.session_id=? AND m.id=? AND `+searchEligible, sessionID, id, owner)
	message, err := scanSearchMessage(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &message, nil
}

func (s *ProfileStore) windowMessages(ctx context.Context, owner, sessionID string, anchorID int64, limit int, forward bool) ([]SearchMessage, error) {
	comparison, order := "<", "DESC"
	if forward {
		comparison, order = ">", "ASC"
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT `+searchMessageColumns+searchMessageJoin+`WHERE m.session_id=? AND m.id `+comparison+` ? AND `+searchEligible+` ORDER BY m.id `+order+` LIMIT ?`, sessionID, anchorID, owner, limit)
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
	id, err := s.resolveSearchSession(ctx, owner, sessionID)
	if err != nil {
		return nil, nil, 0, err
	}
	if id == "" {
		return nil, nil, 0, errors.New("session not found in this profile")
	}
	if head < 1 || tail < 0 || head > 100 || tail > 100 {
		return nil, nil, 0, errors.New("invalid transcript bounds")
	}
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m JOIN sessions s ON s.id=m.session_id WHERE m.session_id=? AND `+searchEligible, id, owner).Scan(&count); err != nil {
		return nil, nil, 0, err
	}
	firstLimit := head
	if count <= head+tail {
		firstLimit = count
	}
	first, err = s.transcriptChunk(ctx, owner, id, firstLimit, false)
	if err != nil {
		return nil, nil, 0, err
	}
	if count > head+tail {
		last, err = s.transcriptChunk(ctx, owner, id, tail, true)
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
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT `+searchMessageColumns+searchMessageJoin+`WHERE m.session_id=? AND `+searchEligible+` ORDER BY m.id `+order+` LIMIT ?`, sessionID, owner, limit)
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
	return s.recentSessions(ctx, owner, "", limit)
}

func (s *ProfileStore) recentSessions(ctx context.Context, owner, liveSessionID string, limit int) ([]SessionSummaryRecord, error) {
	if owner != s.profile {
		return nil, errors.New("invalid profile search scope")
	}
	if limit <= 0 || limit > 40 {
		limit = 8
	}
	roots, err := s.searchLineageRoots(ctx, owner)
	if err != nil {
		return nil, err
	}
	// Only the live session and the sessions forked from it are omitted;
	// older generations are delivered history and remain browsable.
	subtree, err := s.lineageSubtree(ctx, owner, liveSessionID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
SELECT s.id, s.source, COALESCE(s.model,''), COALESCE(s.title,''),
       s.started_at, COALESCE(s.last_activity_at, s.started_at), (SELECT COUNT(*) FROM messages m WHERE m.session_id=s.id AND m.active=1),
       COALESCE((SELECT m.content FROM messages m WHERE m.session_id=s.id AND m.active=1 ORDER BY m.id LIMIT 1),'')
FROM sessions s
WHERE s.profile_name=? AND s.source IN ('discord','imessage')
AND EXISTS(SELECT 1 FROM messages m WHERE m.session_id=s.id AND m.active=1)
ORDER BY COALESCE(s.last_activity_at, s.started_at) DESC,s.id`, owner)
	if err != nil {
		return nil, err
	}
	var records []SessionSummaryRecord
	seen := make(map[string]bool)
	for rows.Next() {
		var record SessionSummaryRecord
		var started, last float64
		if err := rows.Scan(&record.SessionID, &record.Source, &record.Model, &record.Title, &started, &last, &record.MessageCount, &record.Preview); err != nil {
			rows.Close()
			return nil, err
		}
		record.StartedAt = secondsToTime(started)
		record.LastActive = secondsToTime(last)
		root := roots[record.SessionID]
		if root == "" || seen[root] || subtree[record.SessionID] {
			continue
		}
		seen[root] = true
		records = append(records, record)
		if len(records) == limit {
			break
		}
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
	var encoded string
	if err := scan(&message.ID, &message.Role, &message.Content, &message.ToolName, &message.ToolCalls, &message.ToolCallID, &timestamp, &encoded); err != nil {
		return message, err
	}
	message.Timestamp = secondsToTime(timestamp)
	if encoded != "" {
		history, err := searchExchangeHistory(encoded)
		if err != nil {
			return message, err
		}
		message.History = history
	}
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
	resolved, err := s.resolveSearchSession(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if resolved == "" {
		return nil, errors.New("session not found in this profile")
	}
	return s.sessionSummary(ctx, owner, resolved)
}

// resolveSearchSession resolves a concrete id (or existing conversation key),
// never its parent: lineage is for deduplication, not transcript redirection.
func (s *ProfileStore) resolveSearchSession(ctx context.Context, owner, id string) (string, error) {
	var resolved string
	err := s.db.SQL().QueryRowContext(ctx, `SELECT id FROM sessions WHERE profile_name=? AND (id=? OR session_key=?) ORDER BY (id=?) DESC,started_at DESC LIMIT 1`, owner, id, id, id).Scan(&resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return resolved, err
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
// conversation and its forks omitted. Lineage mates dedupe to one entry per
// lineage root.
func (s *ProfileStore) RecentSessionsExcluding(ctx context.Context, owner, liveSessionID string, limit int) ([]SessionSummaryRecord, error) {
	return s.recentSessions(ctx, owner, liveSessionID, limit)
}
