package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *ProfileStore) markDelivery(ctx context.Context, owner string, turnID int64, delivered bool) (resultErr error) {
	defer s.measure("memory.profile.delivery.complete", time.Now(), &resultErr)
	if owner != s.profile || turnID <= 0 {
		return errors.New("invalid profile delivery scope")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, key, encoded string
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT s.id,s.session_key,json_extract(s.origin_json,'$.generation'),v.value FROM messages m JOIN sessions s ON s.id=m.session_id JOIN state_meta v ON v.key=('oswald:v1:turn:'||s.id||':'||m.id) WHERE m.id=? AND m.role='assistant' AND s.profile_name=?`, turnID, owner).Scan(&id, &key, &generation, &encoded); err != nil {
		return err
	}
	activeID, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return err
	}
	if activeID != id {
		return sql.ErrNoRows
	}
	state, err := decodeProfileExchange(encoded)
	if err != nil {
		return err
	}
	// A late success may repair a failed send; a failure cannot undo success.
	if state.Delivery == "delivered" {
		return tx.Commit()
	}
	if delivered && state.Delivery == "failed" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE key=? AND json_extract(value,'$.summary.CoveredThroughTurnID')>=?`, profileSummaryKey(id), turnID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE substr(key,1,?)=?`, len("oswald:v1:compression:"+id+":"), "oswald:v1:compression:"+id+":"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM compression_locks WHERE session_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE substr(key,1,?)=?`, len("oswald:v1:campaign:"+id+":"), "oswald:v1:campaign:"+id+":"); err != nil {
			return err
		}
	}
	state.Delivery = "failed"
	active := 0
	if delivered {
		state.Delivery = "delivered"
		active = 1
	}
	encodedState, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// One exchange owns the contiguous id range from its user row through its
	// final assistant row, including intermediate tool transcript rows. The
	// broker serializes one execution per conversation, so no same-session
	// rows can interleave this range; the session_id predicate isolates
	// globally interleaved rows from other sessions.
	updated, err := tx.ExecContext(ctx, `UPDATE messages SET active=? WHERE session_id=? AND id>=? AND id<=?`, active, id, state.UserMessageID, turnID)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count < 2 || count != turnID-state.UserMessageID+1 {
		return errors.New("incomplete profile exchange")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=? WHERE key=?`, string(encodedState), exchangeKey(id, turnID)); err != nil {
		return err
	}
	if delivered {
		if err := s.refreshSessionLedger(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// mergeSessionOrigin records gateway conversation identity into the session
// origin record while preserving the generation fence fields it already
// carries. Operator data holds the same platform keys; any other keys already
// present are retained byte-indifferently.

func (s *ProfileStore) MarkSessionTurnDelivered(ctx context.Context, owner string, turnID int64) error {
	return s.markDelivery(ctx, owner, turnID, true)
}

// MarkSessionTurnDeliveryFailed keeps the exchange's rows ineligible for prompt history.
func (s *ProfileStore) MarkSessionTurnDeliveryFailed(ctx context.Context, owner string, turnID int64) error {
	return s.markDelivery(ctx, owner, turnID, false)
}

// PageDeliveredSessionTurnsAfter pages complete exchanges in ascending order.
// Supplied FTS triggers index all message rows; future search consumers must also
// apply this delivery/generation gate rather than trusting an FTS hit alone.
func (s *ProfileStore) PageDeliveredSessionTurnsAfter(ctx context.Context, owner, key string, generation int, after int64, limit int) (_ []SessionTurn, resultErr error) {
	return s.deliveredProfileTurns(ctx, owner, key, generation, after, limit, false)
}

// MarkSessionTurnsCompacted flags delivered exchanges as compacted so later
// context reads omit them. The newest turns are expected to be withheld by the
// caller; only the given assistant turn IDs in the active generation are
// touched. Missing turns (expired or other generations) are skipped.
func (s *ProfileStore) MarkSessionTurnsCompacted(ctx context.Context, owner, key string, generation int, turnIDs []int64) error {
	if len(turnIDs) == 0 {
		return nil
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return err
	}
	seen := make(map[int64]struct{}, len(turnIDs))
	for _, turnID := range turnIDs {
		if turnID <= 0 {
			continue
		}
		if _, dup := seen[turnID]; dup {
			continue
		}
		seen[turnID] = struct{}{}
		var userMsgID int64
		err := tx.QueryRowContext(ctx, `SELECT json_extract(value,'$.user_message_id') FROM state_meta WHERE key=?`, exchangeKey(id, turnID)).Scan(&userMsgID)
		if err != nil {
			continue
		}
		if userMsgID <= 0 || userMsgID > turnID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET compacted=1 WHERE session_id=? AND id>=? AND id<=?`, id, userMsgID, turnID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *ProfileStore) deliveredProfileTurns(ctx context.Context, owner, key string, generation int, after int64, limit int, descending bool) ([]SessionTurn, error) {
	if after < 0 {
		return nil, errors.New("invalid profile history boundary")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return nil, err
	}
	order := "ASC"
	if descending {
		order = "DESC"
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,u.content,a.content,v.value FROM messages a
 JOIN state_meta v ON v.key=('oswald:v1:turn:'||a.session_id||':'||a.id)
 JOIN messages u ON u.id=json_extract(v.value,'$.user_message_id') AND u.session_id=a.session_id AND u.role='user' AND u.active=1 AND u.compacted=0
 WHERE a.session_id=? AND a.role='assistant' AND a.active=1 AND a.compacted=0 AND a.id>?
 AND json_extract(v.value,'$.delivery')='delivered' ORDER BY a.id `+order+` LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	var turns []SessionTurn
	for rows.Next() {
		var turn SessionTurn
		var encoded string
		if err := rows.Scan(&turn.ID, &turn.UserText, &turn.AssistantText, &encoded); err != nil {
			rows.Close()
			return nil, err
		}
		state, err := decodeProfileExchange(encoded)
		if err != nil {
			rows.Close()
			return nil, err
		}
		turn.ToolHistory, err = DecodeToolHistory(state.History)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode profile exchange history: %w", err)
		}
		turn.ToolNames = state.ToolNames
		turn.SessionID, turn.UserID, turn.Generation = key, owner, generation
		turns = append(turns, turn)
	}
	iterationErr := rows.Err()
	rows.Close()
	if iterationErr != nil {
		return nil, iterationErr
	}
	return turns, tx.Commit()
}
