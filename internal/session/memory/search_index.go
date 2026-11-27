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

// HidesLive reports whether id is the live session or descended from it.
// Older generations predate the live conversation and stay visible; only the
// live slice and its forks hide, since that slice is already in context.

func (s *ProfileStore) HidesLive(ctx context.Context, owner, live, id string) (bool, error) {
	if owner != s.profile {
		return false, errors.New("invalid profile search scope")
	}
	if strings.TrimSpace(live) == "" {
		return false, nil
	}
	subtree, err := s.lineageSubtree(ctx, owner, live)
	if err != nil {
		return false, err
	}
	resolved, err := s.resolveSearchSession(ctx, owner, id)
	if err != nil || resolved == "" {
		return false, err
	}
	return subtree[resolved], nil
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

// lineageRoot walks parents to the oldest related session stored for owner.
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
	var title, model, displayName, sessionKey, chatName sql.NullString
	err := s.db.SQL().QueryRowContext(ctx, `SELECT s.id,s.source,COALESCE(s.model,''),COALESCE(s.title,''),s.started_at,COALESCE(s.last_activity_at,s.started_at),(SELECT COUNT(*) FROM messages m WHERE m.session_id=s.id),COALESCE(s.display_name,''),COALESCE(s.session_key,''),COALESCE(json_extract(s.origin_json,'$.chat_name'),'') FROM sessions s WHERE s.id=? AND s.profile_name=?`, id, owner).Scan(&summary.SessionID, &summary.Source, &model, &title, &started, &last, &summary.MessageCount, &displayName, &sessionKey, &chatName)
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
	summary.Sender, summary.Scope = senderScope(displayName.String, sessionKey.String)
	summary.ChatName = chatName.String
	return &summary, nil
}

// senderScope derives model-facing attribution from a session row. Sender
// prefers the stored sender display name and falls back to the session key
// sender segment. Scope is "dm" for direct conversations, "group:<chat>" for
// group conversations, and "" when the key carries no conversation shape.

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

// RecentSessionsExcluding returns recent stored sessions with the live
// conversation and its forks omitted. Lineage mates dedupe to one entry per
// lineage root. When groupSource and groupID name the live group conversation,
// the owner's scope narrows to their DMs plus that group.
