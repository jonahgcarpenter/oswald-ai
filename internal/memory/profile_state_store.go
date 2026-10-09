package memory

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
	"github.com/jonahgcarpenter/oswald-ai/internal/media/imagecache"
)

// ProfileStore owns one manually provisioned profile's approved state.db. It
// never resolves accounts or creates profiles, and cannot serve another owner.
type ProfileStore struct {
	db      *database.DB
	profile string
	log     *config.Logger
	now     func() time.Time
	cache   *imagecache.Cache
}

type profileSessionOrigin struct {
	Version    int     `json:"version"`
	Generation int     `json:"generation"`
	TTLSeconds float64 `json:"ttl_seconds"`
}

type profileFileSnapshot struct {
	Version int    `json:"version"`
	User    string `json:"user"`
	Memory  string `json:"memory"`
}

// profileExchangeState uses one bounded state_meta value per exchange. Message
// rows hold the text; assistant message IDs are stable source-turn identifiers.
type profileExchangeState struct {
	Version       int                   `json:"version"`
	UserMessageID int64                 `json:"user_message_id"`
	History       string                `json:"history"`
	ToolNames     []string              `json:"tool_names"`
	Pressure      SessionPromptPressure `json:"pressure"`
	Delivery      string                `json:"delivery"`
	Images        []profileImage        `json:"images,omitempty"`
}

// NewProfileStore opens state.db below an existing, trusted profile directory.
func NewProfileStore(ctx context.Context, root, profile string, log *config.Logger) (*ProfileStore, error) {
	if profile != "default" && !config.ValidProfileName(profile) {
		return nil, errors.New("invalid profile store owner")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("profile state directory is not private")
	}
	db, err := database.OpenState(ctx, filepath.Join(root, "state.db"), log)
	if err != nil {
		return nil, err
	}
	var incompatible int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE profile_name IS NULL OR profile_name<>?`, profile).Scan(&incompatible); err != nil {
		db.Close()
		return nil, err
	}
	if incompatible != 0 {
		db.Close()
		return nil, errors.New("profile database ownership mismatch")
	}
	return &ProfileStore{db: db, profile: profile, log: log, now: time.Now, cache: imagecache.NewProfileCache(root, profile)}, nil
}

// Close releases this profile's database handle after its workers have stopped.
func (s *ProfileStore) Close() error { return s.db.Close() }

func (s *ProfileStore) measure(event string, started time.Time, err *error) {
	if s.log == nil {
		return
	}
	status := "ok"
	outcome := "completed"
	if *err != nil {
		status = "error"
		outcome = "failed"
		if errors.Is(*err, context.Canceled) {
			status = "rejected"
			outcome = "canceled"
		}
	}
	fields := []config.Field{config.F("record_kind", "measurement"), config.F("user_id", s.profile), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", status), config.F("outcome", outcome), config.ErrorField(*err)}
	log := s.log.Server("memory.profile")
	switch event {
	case "memory.profile.session.complete":
		log.Info("memory.profile.session.complete", "resolved profile session", fields...)
	case "memory.profile.reset.complete":
		log.Info("memory.profile.reset.complete", "reset profile session", fields...)
	case "memory.profile.snapshot.complete":
		log.Info("memory.profile.snapshot.complete", "bound profile file snapshot", fields...)
	case "memory.profile.turn.complete":
		log.Info("memory.profile.turn.complete", "stored pending profile exchange", fields...)
	case "memory.profile.delivery.complete":
		log.Info("memory.profile.delivery.complete", "recorded profile exchange delivery", fields...)
	}
}

func (s *ProfileStore) scope(owner, key string) (string, error) {
	if owner != s.profile || strings.TrimSpace(key) != key || key == "" || len(key) > 1024 {
		return "", errors.New("invalid profile session scope")
	}
	source, _, ok := strings.Cut(key, ":")
	if !ok || (source != "discord" && source != "imessage") {
		return "", errors.New("invalid profile session source")
	}
	return source, nil
}

func snapshotKey(id string) string { return "oswald:v1:files:" + id }
func exchangeKey(id string, turn int64) string {
	return "oswald:v1:turn:" + id + ":" + strconv.FormatInt(turn, 10)
}

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
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id,source,user_id,session_key,profile_name,transport_profile,started_at,last_activity_at,origin_json) VALUES(?,?,?,?,?,?,?,?,?)`, config.NewRequestID(), source, owner, key, owner, owner, now, now, string(origin)); err != nil {
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

// AppendPendingSessionTurn writes two inactive message rows and one bounded,
// versioned delivery/history record, including private cache image references.
func (s *ProfileStore) AppendPendingSessionTurn(ctx context.Context, input SessionTurnWrite) (result StoredSessionTurn, resultErr error) {
	defer s.measure("memory.profile.turn.complete", time.Now(), &resultErr)
	if len(input.Images) > 4 {
		return result, errors.New("unsupported profile turn artifacts")
	}
	if !utf8.ValidString(input.UserText) || !utf8.ValidString(input.AssistantText) || len(input.UserText) > 128*1024 || len(input.AssistantText) > 128*1024 || strings.TrimSpace(input.AssistantText) == "" {
		return result, errors.New("invalid profile exchange text")
	}
	if input.Pressure.Tokens < 0 || input.Pressure.Limit <= 0 || input.Pressure.Version == "" || len(input.Pressure.Version) > 1024 {
		return result, errors.New("invalid profile exchange pressure")
	}
	trace, _, err := EncodeToolHistory(input.History)
	if err != nil {
		return result, err
	}
	toolNames := input.ToolNames
	if len(input.History.Batches) > 0 {
		toolNames = successfulToolHistoryNames(input.History)
	}
	if len(toolNames) > 50 {
		return result, errors.New("too many profile exchange tools")
	}
	for _, name := range toolNames {
		if len(name) > 128 {
			return result, errors.New("invalid profile exchange tool name")
		}
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	id, err := s.activeSession(ctx, tx, input.UserID, input.SessionID, input.Generation)
	if err != nil {
		return result, err
	}
	now := s.now().UTC()
	seconds := float64(now.UnixNano()) / 1e9
	userRow, err := tx.ExecContext(ctx, `INSERT INTO messages(session_id,role,content,timestamp,active) VALUES(?,'user',?,?,0)`, id, input.UserText, seconds)
	if err != nil {
		return result, err
	}
	userMessageID, err := userRow.LastInsertId()
	if err != nil {
		return result, err
	}
	assistantRow, err := tx.ExecContext(ctx, `INSERT INTO messages(session_id,role,content,timestamp,active) VALUES(?,'assistant',?,?,0)`, id, input.AssistantText, seconds)
	if err != nil {
		return result, err
	}
	turnID, err := assistantRow.LastInsertId()
	if err != nil {
		return result, err
	}
	state := profileExchangeState{Version: 1, UserMessageID: userMessageID, History: trace, ToolNames: uniqueStrings(toolNames), Pressure: input.Pressure, Delivery: "pending"}
	for _, image := range input.Images {
		data, err := base64.StdEncoding.DecodeString(image.Data)
		if err != nil || len(data) > 280*1024 || image.ImageID == "" || len(image.ImageID) > 64 || image.Version <= 0 {
			return result, errors.New("invalid profile image")
		}
		// Preserve the model-visible selector so a later explicit edit of the
		// delivered path retains logical identity and its version high-water.
		path := image.Path
		if path != "" {
			if _, _, err := s.cache.Resolve(ctx, s.profile, path); err != nil {
				return result, err
			}
		} else {
			path, err = s.cache.Save(ctx, s.profile, data, image.MIMEType)
			if err != nil {
				return result, err
			}
		}
		state.Images = append(state.Images, profileImage{ID: image.ID, ImageID: image.ImageID, Version: image.Version, Highwater: image.VersionHighwater, Parent: image.ParentSourceImageID, Path: path, MIMEType: image.MIMEType})
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return result, err
	}
	if len(encoded) > 256*1024 {
		return result, errors.New("profile exchange metadata exceeds limit")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?)`, exchangeKey(id, turnID), string(encoded)); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET message_count=message_count+2,last_activity_at=? WHERE id=?`, seconds, id); err != nil {
		return result, err
	}
	if err := s.boundImages(ctx, tx, id); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return StoredSessionTurn{ID: turnID, UserID: input.UserID, SessionID: input.SessionID, Generation: input.Generation, UserText: input.UserText, AssistantResponse: input.AssistantText, CreatedAt: now}, nil
}

func decodeProfileExchange(encoded string) (profileExchangeState, error) {
	var state profileExchangeState
	if len(encoded) > 256*1024 || json.Unmarshal([]byte(encoded), &state) != nil || state.Version != 1 || state.UserMessageID <= 0 || (state.Delivery != "pending" && state.Delivery != "failed" && state.Delivery != "delivered") {
		return state, errors.New("invalid profile exchange metadata")
	}
	return state, nil
}

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
	updated, err := tx.ExecContext(ctx, `UPDATE messages SET active=? WHERE session_id=? AND ((id=? AND role='user') OR (id=? AND role='assistant'))`, active, id, state.UserMessageID, turnID)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 2 {
		return errors.New("incomplete profile exchange")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE state_meta SET value=? WHERE key=?`, string(encodedState), exchangeKey(id, turnID)); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkSessionTurnDelivered publishes both messages together, including late success.
func (s *ProfileStore) MarkSessionTurnDelivered(ctx context.Context, owner string, turnID int64) error {
	return s.markDelivery(ctx, owner, turnID, true)
}

// MarkSessionTurnDeliveryFailed keeps both messages ineligible for prompt history.
func (s *ProfileStore) MarkSessionTurnDeliveryFailed(ctx context.Context, owner string, turnID int64) error {
	return s.markDelivery(ctx, owner, turnID, false)
}

// PageDeliveredSessionTurnsAfter pages complete exchanges in ascending order.
// Supplied FTS triggers index all message rows; future search consumers must also
// apply this delivery/generation gate rather than trusting an FTS hit alone.
func (s *ProfileStore) PageDeliveredSessionTurnsAfter(ctx context.Context, owner, key string, generation int, after int64, limit int) (_ []SessionTurn, resultErr error) {
	return s.deliveredProfileTurns(ctx, owner, key, generation, after, limit, false)
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
 JOIN messages u ON u.id=json_extract(v.value,'$.user_message_id') AND u.session_id=a.session_id AND u.role='user' AND u.active=1
 WHERE a.session_id=? AND a.role='assistant' AND a.active=1 AND a.id>?
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
