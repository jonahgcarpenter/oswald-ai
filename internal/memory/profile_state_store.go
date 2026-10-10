package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	fields := []config.Field{config.F("profile", s.profile), config.F("duration_ms", time.Since(started).Milliseconds()), config.F("status", status), config.F("outcome", outcome), config.ErrorField(*err)}
	log := s.log.Server("memory.profile")
	switch event {
	case "memory.profile.session.complete":
		log.Debug("memory.profile.session.complete", "resolved profile session", fields...)
	case "memory.profile.reset.complete":
		log.Debug("memory.profile.reset.complete", "reset profile session", fields...)
	case "memory.profile.snapshot.complete":
		log.Debug("memory.profile.snapshot.complete", "bound profile file snapshot", fields...)
	case "memory.profile.turn.complete":
		log.Debug("memory.profile.turn.complete", "stored pending profile exchange", fields...)
	case "memory.profile.delivery.complete":
		log.Debug("memory.profile.delivery.complete", "recorded profile exchange delivery", fields...)
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
type messageToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// messageToolCall is one entry of a messages.tool_calls JSON list.
type messageToolCall struct {
	ID       string                  `json:"id"`
	CallID   string                  `json:"call_id"`
	Type     string                  `json:"type"`
	Function messageToolCallFunction `json:"function"`
}

// isHexDigest reports whether value is lowercase hexadecimal, the canonical
// encoding for stored SHA-256 digests.
func isHexDigest(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

// nullString stores empty strings as NULL so sparse detail columns keep the
// operator null distribution instead of empty-string sentinels.
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// displayIdentity hashes the dedupe key tuple (role, content, timestamp,
// tool_call_id, tool_calls, tool_name) with SHA-256. Each text field is framed
// as a big-endian uint32 length followed by its UTF-8 bytes; NULL is the
// reserved length 0xFFFFFFFF so it never collides with an empty string. The
// timestamp hashes as its exact float64 bits, avoiding text-formatting drift.
// A nil text pointer means NULL; role is never NULL.
func displayIdentity(role string, content *string, timestamp float64, toolCallID, toolCalls, toolName *string) []byte {
	sum := sha256.New()
	var length [4]byte
	writeField := func(text *string) {
		if text == nil {
			binary.BigEndian.PutUint32(length[:], 0xFFFFFFFF)
			sum.Write(length[:])
			return
		}
		binary.BigEndian.PutUint32(length[:], uint32(len(*text)))
		sum.Write(length[:])
		sum.Write([]byte(*text))
	}
	writeField(&role)
	writeField(content)
	var timestampBits [8]byte
	binary.BigEndian.PutUint64(timestampBits[:], math.Float64bits(timestamp))
	sum.Write(timestampBits[:])
	writeField(toolCallID)
	writeField(toolCalls)
	writeField(toolName)
	return sum.Sum(nil)
}

// encodeMessageToolCalls renders one assistant tool-request row payload from a
// history batch. Arguments are JSON-encoded exactly once; failures fall back
// to an empty object so one bad argument map cannot fail the whole exchange.
func encodeMessageToolCalls(batch ToolHistoryBatch, batchIndex int) (string, []string, error) {
	entries := make([]messageToolCall, 0, len(batch.Calls))
	providerIDs := make([]string, 0, len(batch.Calls))
	for callIndex, call := range batch.Calls {
		providerID := strings.TrimSpace(call.ProviderCallID)
		if providerID == "" {
			providerID = fmt.Sprintf("call_%d_%d", batchIndex+1, callIndex+1)
		}
		args := call.Arguments
		if args == nil {
			args = map[string]interface{}{}
		}
		encodedArgs, err := json.Marshal(args)
		if err != nil {
			encodedArgs = []byte("{}")
		}
		entries = append(entries, messageToolCall{
			ID:     providerID,
			CallID: providerID,
			Type:   "function",
			Function: messageToolCallFunction{
				Name:      strings.TrimSpace(call.Name),
				Arguments: string(encodedArgs),
			},
		})
		providerIDs = append(providerIDs, providerID)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", nil, fmt.Errorf("encode message tool calls: %w", err)
	}
	return string(encoded), providerIDs, nil
}

// AppendPendingSessionTurn writes one inactive user row, one inactive
// assistant tool-request row per history batch, one inactive tool result row
// per call, one inactive final assistant row, and one bounded, versioned
// delivery/history record, including private cache image references.
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
	if !utf8.ValidString(input.AssistantFinishReason) || len(input.AssistantFinishReason) > 64 {
		return result, errors.New("invalid profile exchange finish reason")
	}
	if !utf8.ValidString(input.AssistantReasoning) || !utf8.ValidString(input.AssistantReasoningContent) {
		return result, errors.New("invalid profile exchange reasoning")
	}
	reasoning := input.AssistantReasoning
	if runeCount := utf8.RuneCountInString(reasoning); runeCount > 32768 {
		reasoning = string([]rune(reasoning)[:32768])
	}
	reasoningContent := input.AssistantReasoningContent
	if runeCount := utf8.RuneCountInString(reasoningContent); runeCount > 32768 {
		reasoningContent = string([]rune(reasoningContent)[:32768])
	}
	if input.AssistantTokenCount < 0 {
		return result, errors.New("invalid profile exchange token count")
	}
	platformMessageID := strings.TrimSpace(input.UserPlatformMessageID)
	if !utf8.ValidString(platformMessageID) || len(platformMessageID) > 512 {
		return result, errors.New("invalid profile exchange platform message id")
	}
	model := strings.TrimSpace(input.Model)
	if !utf8.ValidString(model) || len(model) > 256 {
		return result, errors.New("invalid profile exchange model")
	}
	for _, field := range []string{input.BillingProvider, input.BillingBaseURL} {
		if !utf8.ValidString(field) || len(field) > 512 {
			return result, errors.New("invalid profile exchange billing scope")
		}
	}
	if input.ModelConfig != "" && (!utf8.ValidString(input.ModelConfig) || len(input.ModelConfig) > 8192 || !json.Valid([]byte(input.ModelConfig))) {
		return result, errors.New("invalid profile exchange model config")
	}
	chatID := strings.TrimSpace(input.ChatID)
	chatType := strings.TrimSpace(input.ChatType)
	chatDisplayName := strings.TrimSpace(input.ChatDisplayName)
	platform := strings.TrimSpace(input.Platform)
	if !utf8.ValidString(chatID) || len(chatID) > 512 || !utf8.ValidString(chatType) || len(chatType) > 32 {
		return result, errors.New("invalid profile exchange chat identity")
	}
	if chatType != "" && chatType != "dm" && chatType != "group" {
		return result, errors.New("invalid profile exchange chat type")
	}
	if !utf8.ValidString(chatDisplayName) || utf8.RuneCountInString(chatDisplayName) > 256 {
		return result, errors.New("invalid profile exchange chat display name")
	}
	if !utf8.ValidString(platform) || len(platform) > 64 {
		return result, errors.New("invalid profile exchange platform")
	}
	transportProfile := strings.TrimSpace(input.TransportProfile)
	if transportProfile != "" && transportProfile != "default" && !config.ValidProfileName(transportProfile) {
		return result, errors.New("invalid profile exchange transport profile")
	}
	platformUserID := strings.TrimSpace(input.PlatformUserID)
	if !utf8.ValidString(platformUserID) || len(platformUserID) > 512 {
		return result, errors.New("invalid profile exchange platform user id")
	}
	platformDisplayName := strings.TrimSpace(input.PlatformDisplayName)
	if !utf8.ValidString(platformDisplayName) || utf8.RuneCountInString(platformDisplayName) > 256 {
		return result, errors.New("invalid profile exchange platform display name")
	}
	systemPromptHash := strings.TrimSpace(input.SystemPromptHash)
	if systemPromptHash != "" && (len(systemPromptHash) != 64 || !isHexDigest(systemPromptHash)) {
		return result, errors.New("invalid profile exchange system prompt hash")
	}
	if systemPromptHash != "" && !utf8.ValidString(input.SystemPromptText) {
		return result, errors.New("invalid profile exchange system prompt text")
	}
	if systemPromptHash == "" && input.SystemPromptText != "" {
		return result, errors.New("invalid profile exchange system prompt text")
	}
	if len(input.SystemPromptText) > 1024*1024 {
		return result, errors.New("invalid profile exchange system prompt text")
	}
	if !utf8.ValidString(input.ActivityDescription) || utf8.RuneCountInString(input.ActivityDescription) > 256 {
		return result, errors.New("invalid profile exchange activity description")
	}
	for _, batch := range input.History.Batches {
		if !utf8.ValidString(batch.AssistantContent) || len(batch.AssistantContent) > 128*1024 {
			return result, errors.New("invalid profile exchange tool content")
		}
		for _, call := range batch.Calls {
			if !utf8.ValidString(call.Result) {
				return result, errors.New("invalid profile exchange tool result")
			}
		}
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
	userIdentity := displayIdentity("user", &input.UserText, seconds, nil, nil, nil)
	userRow, err := tx.ExecContext(ctx, `INSERT INTO messages(session_id,role,content,platform_message_id,display_identity,timestamp,active) VALUES(?,'user',?,?,?,?,0)`, id, input.UserText, nullString(platformMessageID), userIdentity, seconds)
	if err != nil {
		return result, err
	}
	userMessageID, err := userRow.LastInsertId()
	if err != nil {
		return result, err
	}
	// Persist every model iteration as transcript rows, matching operator
	// data: one assistant tool-request row per history batch and one tool
	// result row per call, all inactive until delivery.
	toolResultRows := 0
	for batchIndex, batch := range input.History.Batches {
		encodedCalls, providerIDs, err := encodeMessageToolCalls(batch, batchIndex)
		if err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(session_id,role,content,tool_calls,finish_reason,display_identity,timestamp,active) VALUES(?,'assistant',?,?,'tool_calls',?,?,0)`, id, batch.AssistantContent, encodedCalls, displayIdentity("assistant", &batch.AssistantContent, seconds, nil, &encodedCalls, nil), seconds); err != nil {
			return result, err
		}
		for callIndex, call := range batch.Calls {
			toolName := strings.TrimSpace(call.Name)
			providerID := providerIDs[callIndex]
			callResult := call.Result
			if _, err := tx.ExecContext(ctx, `INSERT INTO messages(session_id,role,content,tool_call_id,tool_name,display_identity,timestamp,active) VALUES(?,'tool',?,?,?,?,?,0)`, id, call.Result, providerIDs[callIndex], toolName, displayIdentity("tool", &callResult, seconds, &providerID, nil, &toolName), seconds); err != nil {
				return result, err
			}
			toolResultRows++
		}
	}
	finishReason := strings.TrimSpace(input.AssistantFinishReason)
	if finishReason == "" {
		finishReason = "stop"
	}
	var tokenCount any
	if input.AssistantTokenCount > 0 {
		tokenCount = input.AssistantTokenCount
	}
	assistantRow, err := tx.ExecContext(ctx, `INSERT INTO messages(session_id,role,content,finish_reason,reasoning,reasoning_content,token_count,display_identity,timestamp,active) VALUES(?,'assistant',?,?,?,?,?,?,?,0)`, id, input.AssistantText, finishReason, nullString(reasoning), nullString(reasoningContent), tokenCount, displayIdentity("assistant", &input.AssistantText, seconds, nil, nil, nil), seconds)
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
	// Ledger the rendered system prompt before referencing it: the sessions
	// foreign key is enforced per statement. Rows dedupe on the hash.
	if systemPromptHash != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO system_prompts(hash,prompt) VALUES(?,?) ON CONFLICT(hash) DO NOTHING`, systemPromptHash, input.SystemPromptText); err != nil {
			return result, err
		}
	}
	// One exchange owns the contiguous id range from its user row through its
	// final assistant row: the broker serializes one execution per
	// conversation, so no same-session rows can interleave this range.
	totalRows := int64(2 + len(input.History.Batches) + toolResultRows)
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET message_count=message_count+?,tool_call_count=tool_call_count+?,last_activity_at=?,model=?,model_config=?,billing_provider=?,billing_base_url=?,chat_id=?,chat_type=?,user_id=?,display_name=?,transport_profile=?,system_prompt_hash=?,last_activity_description=?,last_activity_provenance='unknown' WHERE id=?`,
		totalRows, toolResultRows, seconds,
		nullString(model), nullString(input.ModelConfig), nullString(strings.TrimSpace(input.BillingProvider)), nullString(strings.TrimSpace(input.BillingBaseURL)),
		nullString(chatID), nullString(chatType), nullString(platformUserID), nullString(platformDisplayName), nullString(transportProfile),
		nullString(systemPromptHash), nullString(strings.TrimSpace(input.ActivityDescription)), id); err != nil {
		return result, err
	}
	if platform != "" || chatID != "" || platformUserID != "" {
		if err := s.mergeSessionOrigin(ctx, tx, id, input.UserID, platform, chatID, chatType, chatDisplayName, platformUserID, platformDisplayName); err != nil {
			return result, err
		}
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
