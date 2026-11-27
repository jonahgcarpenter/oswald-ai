package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *ProfileStore) activeSession(ctx context.Context, tx *sql.Tx, owner, key string, generation int) (string, error) {
	source, err := s.scope(owner, key)
	if err != nil || generation <= 0 {
		return "", errors.New("invalid active profile session scope")
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT s.id FROM sessions s JOIN conversation_generations g
 ON g.source=s.source AND g.session_key=s.session_key
 WHERE s.profile_name=? AND s.source=? AND s.session_key=? AND s.ended_at IS NULL
 AND g.generation=? AND json_extract(s.origin_json,'$.version')=1
 AND json_extract(s.origin_json,'$.generation')=g.generation
 AND s.last_activity_at+json_extract(s.origin_json,'$.ttl_seconds')>?
 ORDER BY s.started_at DESC LIMIT 1`, owner, source, key, generation, float64(s.now().UnixNano())/1e9).Scan(&id)
	return id, err
}

// ResolveSessionContext refreshes one active generation without account tables.
// Expired generations are ended, never revived; reset generations survive reopen.
func (s *ProfileStore) ResolveSessionContext(ctx context.Context, owner, key string, ttl time.Duration) (result SessionContext, resultErr error) {
	defer s.measure("memory.profile.session.complete", time.Now(), &resultErr)
	source, err := s.scope(owner, key)
	if err != nil {
		return result, err
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	now := float64(s.now().UnixNano()) / 1e9
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_generations(source,session_key,generation) VALUES(?,?,1) ON CONFLICT DO NOTHING`, source, key); err != nil {
		return result, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM conversation_generations WHERE source=? AND session_key=?`, source, key).Scan(&result.Generation); err != nil {
		return result, err
	}
	if result.Generation <= 0 {
		return result, errors.New("invalid profile session generation")
	}
	id, err := s.activeSession(ctx, tx, owner, key, result.Generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err == nil {
		var startedAt float64
		if err := tx.QueryRowContext(ctx, `SELECT started_at FROM sessions WHERE id=?`, id).Scan(&startedAt); err != nil {
			return result, err
		}
		result.StartedAt = secondsToTime(startedAt)
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_activity_at=? WHERE id=?`, now, id); err != nil {
			return result, err
		}
	} else {
		// An expired open session advances the high-water generation once. Reset
		// has already advanced it and ended the previous session in one transaction.
		ended, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at=?,end_reason='expired' WHERE profile_name=? AND source=? AND session_key=? AND ended_at IS NULL`, now, owner, source, key)
		if err != nil {
			return result, err
		}
		count, err := ended.RowsAffected()
		if err != nil {
			return result, err
		}
		if count == 0 {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE profile_name=? AND source=? AND session_key=? AND end_reason='expired' AND json_extract(origin_json,'$.generation')=?`, owner, source, key, result.Generation).Scan(&count); err != nil {
				return result, err
			}
		}
		if count > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE conversation_generations SET generation=generation+1 WHERE source=? AND session_key=?`, source, key); err != nil {
				return result, err
			}
			if err := tx.QueryRowContext(ctx, `SELECT generation FROM conversation_generations WHERE source=? AND session_key=?`, source, key).Scan(&result.Generation); err != nil {
				return result, err
			}
		}
		origin, err := json.Marshal(profileSessionOrigin{Version: 1, Generation: result.Generation, TTLSeconds: ttl.Seconds()})
		if err != nil {
			return result, err
		}
		// Link the new session to its most recent ended predecessor, preserving
		// the conversation lineage across resets and expiry.
		var parent any
		var parentID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE profile_name=? AND source=? AND session_key=? AND ended_at IS NOT NULL ORDER BY ended_at DESC, started_at DESC LIMIT 1`, owner, source, key).Scan(&parentID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		} else if err == nil {
			parent = parentID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id,source,user_id,session_key,profile_name,parent_session_id,started_at,last_activity_at,origin_json) VALUES(?,?,?,?,?,?,?,?,?)`, config.NewRequestID(), source, owner, key, owner, parent, now, now, string(origin)); err != nil {
			return result, err
		}
		result.IsNewSession = true
		result.StartedAt = secondsToTime(now)
	}
	result.SpeakerIntro = "You are speaking with " + s.profile + "."
	return result, tx.Commit()
}

// NewSessionContext closes the conversation's current session without deleting
// anything. Transcript, metadata, summaries, and locks stay for later recall and
// ordinary TTL expiry; the next turn opens a fresh session at a new generation.
func (s *ProfileStore) NewSessionContext(ctx context.Context, owner, key string) (resultErr error) {
	defer s.measure("memory.profile.new_session.complete", time.Now(), &resultErr)
	source, err := s.scope(owner, key)
	if err != nil {
		return err
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := float64(s.now().UnixNano()) / 1e9
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_generations(source,session_key,generation) VALUES(?,?,1) ON CONFLICT(source,session_key) DO UPDATE SET generation=generation+1`, source, key); err != nil {
		return err
	}
	// A deliberate tombstone: the transcript is kept, not deleted. Maintained
	// sessions are left unfinalized so ordinary TTL sweep remains their owner.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at=COALESCE(ended_at,?),end_reason=COALESCE(end_reason,'new_session') WHERE profile_name=? AND source=? AND session_key=? AND ended_at IS NULL`, now, owner, source, key); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *ProfileStore) fileSnapshot(ctx context.Context, tx *sql.Tx, id string) (profileFileSnapshot, bool, error) {
	var encoded string
	err := tx.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key=?`, snapshotKey(id)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return profileFileSnapshot{}, false, nil
	}
	if err != nil {
		return profileFileSnapshot{}, false, err
	}
	var snapshot profileFileSnapshot
	var required struct {
		Version int     `json:"version"`
		User    *string `json:"user"`
		Memory  *string `json:"memory"`
	}
	if len(encoded) > 32*1024 || json.Unmarshal([]byte(encoded), &required) != nil || required.Version != 1 || required.User == nil || required.Memory == nil {
		return snapshot, false, errors.New("invalid profile file snapshot")
	}
	snapshot = profileFileSnapshot{Version: required.Version, User: *required.User, Memory: *required.Memory}
	if !validFileSnapshot(snapshot.User, snapshot.Memory) {
		return snapshot, false, errors.New("invalid profile file snapshot")
	}
	return snapshot, true, nil
}

func validFileSnapshot(user, notes string) bool {
	return utf8.ValidString(user) && utf8.ValidString(notes) && utf8.RuneCountInString(user) <= 1375 && utf8.RuneCountInString(notes) <= 2200
}

// SessionFileMemory reads the exact frozen pair for an active generation.
func (s *ProfileStore) SessionFileMemory(ctx context.Context, owner, key string, generation int) (user, notes string, bound bool, resultErr error) {
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return "", "", false, err
	}
	snapshot, bound, err := s.fileSnapshot(ctx, tx, id)
	if err != nil {
		return "", "", false, err
	}
	return snapshot.User, snapshot.Memory, bound, tx.Commit()
}

// UpdateSessionFileMemory overwrites the frozen pair for an active
// generation. It is generation-fenced like BindSessionFileMemory but
// last-writer-wins so mid-turn prompt refreshes persist to later turns.
func (s *ProfileStore) UpdateSessionFileMemory(ctx context.Context, owner, key string, generation int, user, notes string) (_ string, _ string, resultErr error) {
	defer s.measure("memory.profile.snapshot.complete", time.Now(), &resultErr)
	if !validFileSnapshot(user, notes) {
		return "", "", errors.New("invalid profile file snapshot")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(profileFileSnapshot{Version: 1, User: user, Memory: notes})
	if err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, snapshotKey(id), string(encoded)); err != nil {
		return "", "", err
	}
	snapshot, _, err := s.fileSnapshot(ctx, tx, id)
	if err != nil {
		return "", "", err
	}
	return snapshot.User, snapshot.Memory, tx.Commit()
}

// BindSessionFileMemory is generation-fenced, first-writer-wins, and atomic for
// both files, including a captured empty pair.
func (s *ProfileStore) BindSessionFileMemory(ctx context.Context, owner, key string, generation int, user, notes string) (_ string, _ string, resultErr error) {
	defer s.measure("memory.profile.snapshot.complete", time.Now(), &resultErr)
	if !validFileSnapshot(user, notes) {
		return "", "", errors.New("invalid profile file snapshot")
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, owner, key, generation)
	if err != nil {
		return "", "", err
	}
	encoded, err := json.Marshal(profileFileSnapshot{Version: 1, User: user, Memory: notes})
	if err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT DO NOTHING`, snapshotKey(id), string(encoded)); err != nil {
		return "", "", err
	}
	snapshot, _, err := s.fileSnapshot(ctx, tx, id)
	if err != nil {
		return "", "", err
	}
	return snapshot.User, snapshot.Memory, tx.Commit()
}

// messageToolCallFunction is the persisted function reference inside one
// messages.tool_calls JSON entry: provider call id, function name, and
// JSON-encoded arguments, matching the transcript shapes in operator data.

func (s *ProfileStore) mergeSessionOrigin(ctx context.Context, tx *sql.Tx, id, owner, platform, chatID, chatType, chatName, userID, userName string) error {
	var encoded sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT origin_json FROM sessions WHERE id=?`, id).Scan(&encoded); err != nil {
		return err
	}
	record := map[string]any{}
	if encoded.Valid && strings.TrimSpace(encoded.String) != "" {
		if err := json.Unmarshal([]byte(encoded.String), &record); err != nil {
			return errors.New("invalid session origin record")
		}
	}
	set := func(key, value string) {
		if value != "" {
			record[key] = value
		}
	}
	set("platform", platform)
	set("chat_id", chatID)
	set("chat_name", chatName)
	set("chat_type", chatType)
	set("user_id", userID)
	set("user_name", userName)
	record["profile"] = owner
	merged, err := json.Marshal(record)
	if err != nil || len(merged) > 8192 {
		return errors.New("invalid session origin record")
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET origin_json=? WHERE id=?`, string(merged), id)
	return err
}

// refreshSessionLedger recomputes a session's delivered usage counters and
// tool-set hash from durable rows, so late delivery repairs the aggregates.
// Only the main chat task feeds the token counters, matching operator data.
func (s *ProfileStore) refreshSessionLedger(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET
 input_tokens=COALESCE((SELECT SUM(input_tokens) FROM session_model_usage WHERE session_id=? AND task=''),0),
 output_tokens=COALESCE((SELECT SUM(output_tokens) FROM session_model_usage WHERE session_id=? AND task=''),0),
 api_call_count=COALESCE((SELECT SUM(api_call_count) FROM session_model_usage WHERE session_id=? AND task=''),0)
 WHERE id=?`, id, id, id, id); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT tool_name FROM messages WHERE session_id=? AND role='tool' AND tool_name IS NOT NULL AND tool_name<>'' ORDER BY tool_name`, id)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var toolSet any
	if len(names) > 0 {
		encoded, err := json.Marshal(names)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(encoded)
		digest := hex.EncodeToString(sum[:])
		toolSet = digest
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET tool_names=? WHERE id=?`, toolSet, id)
	return err
}

// MarkSessionTurnDelivered publishes the exchange's rows together, including late success.
