package memory

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

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
			toolName := strings.TrimSpace(call.ToolResultName())
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
