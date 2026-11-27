package memory

import (
	"context"
	"database/sql"
	"errors"
)

// SearchSession is one deduplicated session lineage returned by discovery.
// Sender names the human owner of the slice (session display name, falling
// back to the session key sender segment). Scope is "dm" for direct
// conversations or "group:<chat>" for group conversations. ChatName is the
// untrusted plain conversation name from the session origin.

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

// SessionWindowRemainder counts stored messages outside a returned window.
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

// SessionTranscript returns the transcript for one stored session, bounded
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

// RecentSessions returns the most recently active stored sessions.

func (s *ProfileStore) SessionAllMessages(ctx context.Context, owner, sessionID string) ([]SearchMessage, error) {
	if owner != s.profile {
		return nil, errors.New("invalid profile search scope")
	}
	id, err := s.resolveSearchSession(ctx, owner, sessionID)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errors.New("session not found in this profile")
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT `+searchMessageColumns+searchMessageJoin+`WHERE m.session_id=? AND `+searchEligible+` ORDER BY m.id ASC LIMIT ?`, id, owner, maxGroupMergeMessages)
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
	return messages, nil
}
