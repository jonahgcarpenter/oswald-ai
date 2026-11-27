package memory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// SearchSession is one deduplicated session lineage returned by discovery.
// Sender names the human owner of the slice (session display name, falling
// back to the session key sender segment). Scope is "dm" for direct
// conversations or "group:<chat>" for group conversations. ChatName is the
// untrusted plain conversation name from the session origin.

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

// DiscoverySessions runs an FTS5 search over stored messages and returns one
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

func (s *ProfileStore) RecentSessions(ctx context.Context, owner string, limit int) ([]SessionSummaryRecord, error) {
	return s.recentSessions(ctx, owner, "", limit, "", "")
}

func (s *ProfileStore) recentSessions(ctx context.Context, owner, liveSessionID string, limit int, groupSource, groupID string) ([]SessionSummaryRecord, error) {
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
	// older generations are stored history and remain browsable.
	subtree, err := s.lineageSubtree(ctx, owner, liveSessionID)
	if err != nil {
		return nil, err
	}
	query := `
SELECT s.id, s.source, COALESCE(s.model,''), COALESCE(s.title,''),
       s.started_at, COALESCE(s.last_activity_at, s.started_at), (SELECT COUNT(*) FROM messages m WHERE m.session_id=s.id),
       COALESCE((SELECT m.content FROM messages m WHERE m.session_id=s.id ORDER BY m.id LIMIT 1),''),
       COALESCE(s.display_name,''), COALESCE(s.session_key,''), COALESCE(json_extract(s.origin_json,'$.chat_name'),'')
FROM sessions s
WHERE s.profile_name=? AND s.source IN ('discord','imessage')
AND EXISTS(SELECT 1 FROM messages m WHERE m.session_id=s.id)`
	args := []any{owner}
	if groupSource != "" && groupID != "" {
		// Group turn: the owner's DMs plus the live group. Peer DMs and
		// other groups never surface here; peers are fanned out separately.
		query += ` AND (s.session_key LIKE '%:dm:%' ESCAPE '\' OR s.session_key LIKE ? ESCAPE '\')`
		args = append(args, escapeLikePrefix(groupSource)+":"+escapeLikePrefix(groupID)+":%")
	}
	query += `
ORDER BY COALESCE(s.last_activity_at, s.started_at) DESC,s.id`
	rows, err := s.db.SQL().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var records []SessionSummaryRecord
	seen := make(map[string]bool)
	for rows.Next() {
		var record SessionSummaryRecord
		var started, last float64
		var displayName, sessionKey, chatName string
		if err := rows.Scan(&record.SessionID, &record.Source, &record.Model, &record.Title, &started, &last, &record.MessageCount, &record.Preview, &displayName, &sessionKey, &chatName); err != nil {
			rows.Close()
			return nil, err
		}
		record.StartedAt = secondsToTime(started)
		record.LastActive = secondsToTime(last)
		record.Sender, record.Scope = senderScope(displayName, sessionKey)
		record.ChatName = chatName
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

func (s *ProfileStore) RecentSessionsExcluding(ctx context.Context, owner, liveSessionID string, limit int, groupSource, groupID string) ([]SessionSummaryRecord, error) {
	return s.recentSessions(ctx, owner, liveSessionID, limit, groupSource, groupID)
}

// GroupMember is one stored session row participating in a group
// conversation. Sessions stay per-sender; the group view merges them.

func (s *ProfileStore) GroupMemberSessions(ctx context.Context, owner, source, groupID string) ([]GroupMember, error) {
	if owner != s.profile {
		return nil, errors.New("invalid profile search scope")
	}
	if source != "discord" && source != "imessage" {
		return nil, errors.New("invalid group scope")
	}
	if strings.TrimSpace(groupID) == "" || groupID == "dm" {
		return nil, errors.New("invalid group scope")
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT s.id,COALESCE(s.display_name,''),COALESCE(s.session_key,''),COALESCE(json_extract(s.origin_json,'$.chat_name'),'') FROM sessions s WHERE s.profile_name=? AND s.session_key LIKE ? ESCAPE '\' AND EXISTS(SELECT 1 FROM messages m WHERE m.session_id=s.id) ORDER BY COALESCE(s.last_activity_at,s.started_at),s.id`, owner, escapeLikePrefix(source)+":"+escapeLikePrefix(groupID)+":%")
	if err != nil {
		return nil, err
	}
	var members []GroupMember
	for rows.Next() {
		var member GroupMember
		var displayName, sessionKey string
		if err := rows.Scan(&member.SessionID, &displayName, &sessionKey, &member.ChatName); err != nil {
			rows.Close()
			return nil, err
		}
		member.Sender, member.Scope = senderScope(displayName, sessionKey)
		members = append(members, member)
	}
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return nil, iterationErr
	}
	return members, nil
}

// maxGroupMergeMessages bounds one member slice of a merged group view.
const maxGroupMergeMessages = 2000

// SessionAllMessages returns every stored message of one session in id order,
// capped at maxGroupMergeMessages. Group merging sorts member slices by
// timestamp; the envelope budget truncates the rendered result.

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

func (s *ProfileStore) Profile() string { return s.profile }

// SessionSummaryFor returns stored session metadata for one session id.
