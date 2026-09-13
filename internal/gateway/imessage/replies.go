package imessage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

const replyLookupTimeout = 5 * time.Second
const replyLookupBodyLimit = 1 << 20

var errInvalidReplyResponse = errors.New("invalid reply response")

// Select exactly one conversational predecessor before checking authorship. Raw
// SQLite dates preserve nanoseconds; ROWID breaks ties without API sort support.
// The root belongs to its thread even though its own thread origin is NULL.
const threadPredecessorSQL = `message.ROWID = (
 SELECT p.ROWID FROM message AS p JOIN message AS a ON a.guid = :anchor
 WHERE a.ROWID = :anchor_row AND a.date > 0
 AND a.thread_originator_guid = :root
 AND COALESCE(a.thread_originator_part, '') = :part
 AND EXISTS (SELECT 1 FROM chat_message_join aj JOIN chat ac ON ac.ROWID = aj.chat_id WHERE aj.message_id = a.ROWID AND ac.guid = :chat)
 AND EXISTS (SELECT 1 FROM chat_message_join pj JOIN chat pc ON pc.ROWID = pj.chat_id WHERE pj.message_id = p.ROWID AND pc.guid = :chat)
 AND (p.guid = :root OR (p.thread_originator_guid = :root AND COALESCE(p.thread_originator_part, '') = :part))
 AND p.date > 0 AND (p.date < a.date OR (p.date = a.date AND p.ROWID < a.ROWID))
 AND COALESCE(p.associated_message_type, 0) = 0
 AND p.is_system_message = 0 AND p.is_service_message = 0 AND COALESCE(p.item_type, 0) = 0
 AND NOT EXISTS (
  SELECT 1 FROM message u JOIN chat_message_join uj ON uj.message_id = u.ROWID JOIN chat uc ON uc.ROWID = uj.chat_id
   WHERE uc.guid = :chat AND (u.guid = :root OR (u.thread_originator_guid = :root AND COALESCE(u.thread_originator_part, '') = :part))
  AND COALESCE(u.associated_message_type, 0) = 0 AND u.is_system_message = 0 AND u.is_service_message = 0 AND COALESCE(u.item_type, 0) = 0
  AND (u.date IS NULL OR u.date <= 0)
 )
 ORDER BY p.date DESC, p.ROWID DESC LIMIT 1
)`

// resolveReply resolves both admission and enrichment, with one bounded lookup
// lifetime. Cache entries establish direct references only, never thread order.
func (g *Gateway) resolveReply(parent context.Context, msg webhookMessage, allowPredecessor bool, requestID string) (result messageContext, found bool) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, replyLookupTimeout)
	defer cancel()
	status := "ok"
	phase, reasonCode := "reference", "resolved"
	cacheCount, remoteCount, directCount, predecessorCount, notFoundCount, rejectedCount, errorCount := 0, 0, 0, 0, 0, 0, 0
	reject := func(reason string) {
		status, reasonCode = "rejected", reason
		rejectedCount++
	}
	defer func() {
		outcome := "complete"
		if errors.Is(ctx.Err(), context.Canceled) {
			status, outcome, errorCount, rejectedCount = "ok", "canceled", 0, 0
			reasonCode = "canceled"
		}
		g.log().Info("gateway.reply_lookup.complete", "resolved imessage reply context",
			config.F("record_kind", "measurement"), config.F("request_id", requestID), config.F("status", status), config.F("outcome", outcome),
			config.F("phase", phase), config.F("reason_code", reasonCode),
			config.F("duration_ms", time.Since(started).Milliseconds()), config.F("cache_count", cacheCount),
			config.F("remote_count", remoteCount), config.F("direct_count", directCount), config.F("predecessor_count", predecessorCount),
			config.F("not_found_count", notFoundCount), config.F("rejected_count", rejectedCount), config.F("error_count", errorCount))
	}()
	chatGUID := msg.primaryChat().GUID
	// An explicit selected target must not be replaced by a different thread root.
	target := msg.replyTargetGUID()
	if msg.ReplyToGUID != "" {
		target = msg.ReplyToGUID
	}
	if ctx.Err() != nil {
		status, reasonCode, errorCount = "error", "lookup_context_done", 1
		return
	}
	if target == "" || chatGUID == "" {
		reject("missing_reference_scope")
		return
	}
	cached, cachedOK := g.lookupMessage(target)
	if cachedOK && cached.ChatGUID == chatGUID {
		cacheCount++
		if cached.IsFromBot || !allowPredecessor || msg.ThreadOriginatorGUID == "" || target != msg.ThreadOriginatorGUID {
			directCount = 1
			if !cached.IsFromBot {
				reasonCode = "not_eligible_bot"
			}
			return cached, true
		}
	}
	lookup := func(guid string) (messageLookupData, bool) {
		remoteCount++
		data, err := g.fetchReplyMessage(ctx, guid, chatGUID)
		if err != nil {
			status, errorCount = "error", errorCount+1
			reasonCode = "lookup_failed"
			return messageLookupData{}, false
		}
		if data.GUID == "" {
			notFoundCount++
			reasonCode = "message_not_found"
			return data, false
		}
		if data.GUID != guid {
			reject("message_guid_mismatch")
			return messageLookupData{}, false
		}
		if !data.matches(guid, chatGUID) {
			reject("message_chat_mismatch")
			return messageLookupData{}, false
		}
		if data.IsSystemMessage == nil || data.IsServiceMessage == nil {
			reject("message_flags_missing")
			return messageLookupData{}, false
		}
		if !data.conversational() {
			reject("non_conversational_message")
			return messageLookupData{}, false
		}
		return data, true
	}
	data, ok := lookup(target)
	if !ok {
		return
	}
	result, found = g.replyContextFromMessage(data, chatGUID), true
	if result.IsFromBot || !allowPredecessor || msg.ThreadOriginatorGUID == "" || target != msg.ThreadOriginatorGUID {
		directCount = 1
		if !result.IsFromBot {
			reasonCode = "not_eligible_bot"
		}
		return
	}
	// Only a real human root can enable the fallback. Failed bot sends cannot.
	switch {
	case data.IsFromMe:
		reject("root_not_human")
		return
	case data.OriginalROWID <= 0:
		reject("root_row_missing")
		return
	case data.DateCreated == nil || *data.DateCreated <= 0:
		reject("root_date_missing")
		return
	case data.ThreadOriginatorGUID != "" && data.ThreadOriginatorGUID != data.GUID:
		reject("root_thread_mismatch")
		return
	case msg.GUID == "":
		reject("incoming_guid_missing")
		return
	}
	phase = "anchor"
	anchor, ok := lookup(msg.GUID)
	if !ok {
		return
	}
	switch {
	case anchor.OriginalROWID <= 0:
		reject("anchor_row_missing")
		return
	case anchor.DateCreated == nil || *anchor.DateCreated <= 0:
		reject("anchor_date_missing")
		return
	case anchor.IsFromMe:
		reject("anchor_self_authored")
		return
	case anchor.ThreadOriginatorGUID != msg.ThreadOriginatorGUID:
		reject("anchor_thread_mismatch")
		return
	case msg.ThreadOriginatorPart != "" && anchor.ThreadOriginatorPart != msg.ThreadOriginatorPart:
		reject("anchor_part_mismatch")
		return
	}
	if anchor.ReplyToGUID != "" && anchor.ReplyToGUID != target {
		// REST may expose the selected bubble omitted from the webhook. Resolve
		// it directly; never override an explicit human target with a predecessor.
		phase = "direct_target"
		explicit, ok := lookup(anchor.ReplyToGUID)
		if !ok {
			return
		}
		result, found = g.replyContextFromMessage(explicit, chatGUID), true
		directCount = 1
		if !result.IsFromBot {
			reasonCode = "not_eligible_bot"
		}
		return
	}
	phase = "predecessor"
	remoteCount++
	rows, err := g.queryReplyMessages(ctx, messageQueryRequest{
		ChatGUID: chatGUID, Limit: 2, Sort: "DESC", With: []string{"chat", "attachment", "handle"},
		Where: []messageQueryClause{{Statement: threadPredecessorSQL, Args: map[string]string{
			"anchor": msg.GUID, "anchor_row": strconv.FormatInt(anchor.OriginalROWID, 10),
			"root": target, "part": anchor.ThreadOriginatorPart, "chat": chatGUID,
		}}},
	})
	if err != nil {
		status, errorCount = "error", errorCount+1
		reasonCode = "lookup_failed"
		return
	}
	if len(rows) == 0 {
		notFoundCount++
		reasonCode = "predecessor_not_found"
		return
	}
	if len(rows) != 1 {
		reject("predecessor_ambiguous")
		return
	}
	p := rows[0]
	switch {
	case !p.matches(p.GUID, chatGUID):
		reject("predecessor_scope_mismatch")
		return
	case !p.conversational():
		reject("predecessor_not_conversational")
		return
	case p.OriginalROWID <= 0:
		reject("predecessor_row_missing")
		return
	case p.DateCreated == nil || *p.DateCreated <= 0:
		reject("predecessor_date_missing")
		return
	case p.GUID == anchor.GUID || p.OriginalROWID == anchor.OriginalROWID || *p.DateCreated > *anchor.DateCreated:
		reject("predecessor_order_mismatch")
		return
	case p.GUID != target && (p.ThreadOriginatorGUID != target || p.ThreadOriginatorPart != anchor.ThreadOriginatorPart):
		reject("predecessor_thread_mismatch")
		return
	}
	// Millisecond equality cannot verify raw ordering locally; SQL owns that check.
	result = g.replyContextFromMessage(p, chatGUID)
	result.IsPredecessor = true
	predecessorCount = 1
	if !result.IsFromBot {
		reasonCode = "not_eligible_bot"
	}
	return
}

func (data messageLookupData) matches(guid, chatGUID string) bool {
	if guid == "" || data.GUID != guid || chatGUID == "" {
		return false
	}
	for _, chat := range data.Chats {
		if chat.GUID == chatGUID {
			return true
		}
	}
	return false
}

func (data messageLookupData) conversational() bool {
	typ := strings.Trim(string(data.AssociatedMessageType), "\" \t\r\n")
	return data.IsSystemMessage != nil && !*data.IsSystemMessage && data.IsServiceMessage != nil && !*data.IsServiceMessage &&
		data.ItemType == 0 && (typ == "" || typ == "null" || typ == "0")
}

func (g *Gateway) fetchReplyMessage(ctx context.Context, guid, chatGUID string) (messageLookupData, error) {
	rows, err := g.queryReplyMessages(ctx, messageQueryRequest{
		ChatGUID: chatGUID, Limit: 2, With: []string{"chat", "attachment", "handle"},
		// BlueBubbles reserves :guid for its chatGuid predicate.
		Where: []messageQueryClause{{Statement: "message.guid = :reply_guid", Args: map[string]string{"reply_guid": guid}}},
	})
	if errors.Is(err, errInvalidReplyResponse) {
		return messageLookupData{}, err
	}
	if err == nil && len(rows) > 0 {
		if len(rows) != 1 {
			return messageLookupData{}, errors.New("ambiguous reply lookup")
		}
		return rows[0], nil
	}
	if ctx.Err() != nil {
		return messageLookupData{}, ctx.Err()
	}
	endpoint, err := buildBlueBubblesMessageEndpoint(g.BlueBubblesURL, guid, g.BlueBubblesPassword)
	if err != nil {
		return messageLookupData{}, err
	}
	var response messageLookupResponse
	if err := g.replyLookupHTTP(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return messageLookupData{}, err
	}
	if response.Error != nil {
		return messageLookupData{}, errors.New("reply lookup provider error")
	}
	return response.Data, nil
}

func (g *Gateway) queryReplyMessages(ctx context.Context, payload messageQueryRequest) ([]messageLookupData, error) {
	endpoint, err := buildBlueBubblesEndpoint(g.BlueBubblesURL, "/api/v1/message/query", g.BlueBubblesPassword)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var response messageQueryResponse
	if err := g.replyLookupHTTP(ctx, http.MethodPost, endpoint, body, &response); err != nil {
		return nil, err
	}
	if response.Error != nil || len(response.Data) > payload.Limit {
		return nil, errInvalidReplyResponse
	}
	return response.Data, nil
}

func (g *Gateway) replyLookupHTTP(ctx context.Context, method, endpoint string, body []byte, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reply lookup HTTP status %d", resp.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(resp.Body, replyLookupBodyLimit+1))
	if err != nil {
		return err
	}
	if len(encoded) > replyLookupBodyLimit {
		return errInvalidReplyResponse
	}
	if err := json.Unmarshal(encoded, result); err != nil {
		return errInvalidReplyResponse
	}
	return nil
}

func (g *Gateway) replyContextFromMessage(data messageLookupData, chatGUID string) messageContext {
	// Delivery receipts are not required for authorship; group messages may
	// lack them. Explicit errors, corruption, and retraction still block admission.
	ctx := messageContext{
		ChatGUID: chatGUID, Text: strings.TrimSpace(data.Text), Attachments: data.Attachments,
		IsFromBot: data.IsFromMe && data.SendError != nil && *data.SendError == 0 && !data.IsCorrupt &&
			(data.DateRetracted == nil || *data.DateRetracted == 0) &&
			(strings.TrimSpace(data.Text) != "" || len(data.Attachments) > 0),
		CreatedAt: time.Now(),
	}
	if data.IsFromMe {
		ctx.SenderID, ctx.DisplayName = "imessage:self", "Oswald"
	} else {
		ctx.SenderID = strings.TrimSpace(data.Handle.Address)
		if normalized, err := accounts.NormalizeIdentifier("imessage", ctx.SenderID); err == nil {
			ctx.SenderID = normalized
		}
		ctx.DisplayName = ctx.SenderID
		if ctx.DisplayName == "" {
			ctx.DisplayName = "someone"
		}
	}
	return ctx
}

// rememberInboundMessage caches inbound message context for reply reconstruction.
func (g *Gateway) rememberInboundMessage(msg webhookMessage, sessionKey, normalizedSenderID, displayName string) {
	if msg.GUID == "" {
		return
	}
	g.rememberMessage(msg.GUID, messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    msg.primaryChat().GUID,
		SenderID:    normalizedSenderID,
		DisplayName: displayName,
		Text:        strings.TrimSpace(msg.Text),
		Attachments: msg.Attachments,
		IsFromBot:   false,
		CreatedAt:   time.Now(),
	})
}

// rememberBotMessage caches bot-authored message context for reply reconstruction.
func (g *Gateway) rememberBotMessage(messageGUID, sessionKey, chatGUID, senderID, text string) {
	g.rememberMessage(messageGUID, messageContext{
		SessionKey:  sessionKey,
		ChatGUID:    chatGUID,
		SenderID:    senderID,
		DisplayName: "Oswald",
		Text:        strings.TrimSpace(text),
		IsFromBot:   true,
		CreatedAt:   time.Now(),
	})
}

// rememberMessage stores reply context in the in-memory message index.
func (g *Gateway) rememberMessage(messageGUID string, ctx messageContext) {
	if messageGUID == "" {
		return
	}
	g.messageMu.Lock()
	defer g.messageMu.Unlock()
	g.pruneMessageIndexLocked()
	g.messageIndex[messageGUID] = ctx
}

// lookupMessage returns cached reply context for a prior message GUID.
func (g *Gateway) lookupMessage(messageGUID string) (messageContext, bool) {
	if messageGUID == "" {
		return messageContext{}, false
	}
	g.messageMu.Lock()
	defer g.messageMu.Unlock()
	g.pruneMessageIndexLocked()
	ctx, ok := g.messageIndex[messageGUID]
	return ctx, ok
}

// pruneMessageIndexLocked removes expired entries from the in-memory message index.
func (g *Gateway) pruneMessageIndexLocked() {
	cutoff := time.Now().Add(-messageIndexTTL)
	for guid, ctx := range g.messageIndex {
		if ctx.CreatedAt.Before(cutoff) {
			delete(g.messageIndex, guid)
		}
	}
}

// replyTargetGUID returns the thread root, falling back to an explicit target.
func (m webhookMessage) replyTargetGUID() string {
	if m.ThreadOriginatorGUID != "" {
		return m.ThreadOriginatorGUID
	}
	return m.ReplyToGUID
}
