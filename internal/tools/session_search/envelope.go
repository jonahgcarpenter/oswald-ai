package session_search

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
)

const (
	windowContentRunes  = 4000
	messageContentRunes = 2000
	bookendContentRunes = 1200
	// envelopeRunes keeps the tool result inside the durable history bound.
	envelopeRunes = 14000
)

type messageJSON struct {
	ID                   int64   `json:"id"`
	Role                 string  `json:"role"`
	Content              string  `json:"content"`
	ToolName             string  `json:"tool_name,omitempty"`
	ToolCalls            string  `json:"tool_calls,omitempty"`
	Timestamp            float64 `json:"timestamp"`
	Anchor               bool    `json:"anchor,omitempty"`
	ContentTruncated     bool    `json:"content_truncated,omitempty"`
	OriginalContentChars int     `json:"original_content_chars,omitempty"`
}

type discoverResultJSON struct {
	SessionID      string        `json:"session_id"`
	When           string        `json:"when"`
	Source         string        `json:"source"`
	Model          string        `json:"model,omitempty"`
	Title          string        `json:"title,omitempty"`
	MatchedRole    string        `json:"matched_role"`
	MatchMessageID int64         `json:"match_message_id"`
	Snippet        string        `json:"snippet"`
	BookendStart   []messageJSON `json:"bookend_start,omitempty"`
	Messages       []messageJSON `json:"messages"`
	BookendEnd     []messageJSON `json:"bookend_end,omitempty"`
	MessagesBefore int           `json:"messages_before"`
	MessagesAfter  int           `json:"messages_after"`
	Detail         string        `json:"detail"`
	Link           string        `json:"link"`
}

type discoverEnvelope struct {
	Success          bool                 `json:"success"`
	Mode             string               `json:"mode"`
	Query            string               `json:"query"`
	Detail           string               `json:"detail"`
	Count            int                  `json:"count"`
	SessionsSearched int                  `json:"sessions_searched"`
	Results          []discoverResultJSON `json:"results"`
	LinkHint         string               `json:"link_hint"`
	Message          string               `json:"message,omitempty"`
}

type browseResultJSON struct {
	SessionID    string  `json:"session_id"`
	Link         string  `json:"link"`
	Title        string  `json:"title,omitempty"`
	Source       string  `json:"source"`
	StartedAt    float64 `json:"started_at"`
	LastActive   float64 `json:"last_active"`
	MessageCount int     `json:"message_count"`
	Preview      string  `json:"preview"`
}

type browseEnvelope struct {
	Success bool               `json:"success"`
	Mode    string             `json:"mode"`
	Count   int                `json:"count"`
	Results []browseResultJSON `json:"results"`
	Message string             `json:"message"`
}

type readEnvelope struct {
	Success      bool            `json:"success"`
	Mode         string          `json:"mode"`
	SessionID    string          `json:"session_id"`
	Link         string          `json:"link"`
	SessionMeta  sessionMetaJSON `json:"session_meta"`
	MessageCount int             `json:"message_count"`
	Truncated    bool            `json:"truncated"`
	Messages     []messageJSON   `json:"messages"`
	Message      string          `json:"message,omitempty"`
}

type sessionMetaJSON struct {
	When   string `json:"when"`
	Source string `json:"source"`
	Model  string `json:"model,omitempty"`
	Title  string `json:"title,omitempty"`
}

type scrollEnvelope struct {
	Success         bool          `json:"success"`
	Mode            string        `json:"mode"`
	SessionID       string        `json:"session_id"`
	AroundMessageID int64         `json:"around_message_id"`
	Window          int           `json:"window"`
	MessagesBefore  int           `json:"messages_before"`
	MessagesAfter   int           `json:"messages_after"`
	Messages        []messageJSON `json:"messages"`
}

const linkHint = "When referring the user to a session, write its `link` value verbatim inline mid-sentence; it renders as a titled link."

func sessionLink(profile, id string) string { return "@session:" + profile + "/" + id }

// discoverLink uses the owner profile; session ids are opaque to the model.
func discoverLink(profile, id string) string { return sessionLink(profile, id) }

// formatWhen renders a message time as a human-readable UTC label.
func formatWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("January 2, 2006 at 03:04 PM")
}

func toEpoch(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

func buildDiscover(ctx context.Context, store *memory.ProfileStore, owner string, parsed discoveryArgs, sessions []memory.SearchSession) (string, error) {
	envelope := discoverEnvelope{Success: true, Mode: "discover", Query: parsed.Query, Detail: parsed.Detail, Count: len(sessions), SessionsSearched: len(sessions), LinkHint: linkHint, Results: []discoverResultJSON{}}
	for index, session := range sessions {
		result, err := hydrateDiscover(ctx, store, owner, parsed, session, index == 0)
		if err != nil {
			return "", err
		}
		envelope.Results = append(envelope.Results, result)
	}
	if len(sessions) == 0 {
		envelope.Message = "No delivered messages matched. Broaden with FTS5 syntax: OR between synonyms, \"quoted phrases\", NOT to exclude, and prefix* wildcards."
	}
	return marshalBounded(envelope)
}

func hydrateDiscover(ctx context.Context, store *memory.ProfileStore, owner string, parsed discoveryArgs, session memory.SearchSession, top bool) (discoverResultJSON, error) {
	hydrate := top || parsed.Detail == "full"
	result := discoverResultJSON{
		SessionID: session.SessionID, When: formatWhen(session.StartedAt), Source: session.Source,
		Model: session.Model, Title: session.Title, MatchedRole: session.MatchedRole,
		MatchMessageID: session.MatchMessageID, Snippet: session.Snippet,
		Detail: "compact", Link: discoverLink(owner, session.SessionID),
		Messages: []messageJSON{},
	}
	before, anchor, after, err := store.SessionWindow(ctx, owner, session.SessionID, session.MatchMessageID, defaultWindow)
	if err != nil {
		return result, nil
	}
	if !hydrate {
		if anchor != nil {
			result.Messages = []messageJSON{toJSON(*anchor, true, messageContentRunes)}
		}
		return result, nil
	}
	result.Detail = "full"
	result.MessagesBefore = len(before)
	result.MessagesAfter = len(after)
	for _, message := range before {
		result.Messages = append(result.Messages, toJSON(message, false, windowContentRunes))
	}
	if anchor != nil {
		result.Messages = append(result.Messages, toJSON(*anchor, true, windowContentRunes))
	}
	result.BookendStart, result.BookendEnd = bookends(ctx, store, owner, session.SessionID)
	return result, nil
}

// bookends returns the first and last delivered messages of a session.
func bookends(ctx context.Context, store *memory.ProfileStore, owner, sessionID string) ([]messageJSON, []messageJSON) {
	first, last, count, err := store.SessionTranscript(ctx, owner, sessionID, 2, 2)
	if err != nil || count == 0 {
		return nil, nil
	}
	start := make([]messageJSON, 0, len(first))
	for _, message := range first {
		start = append(start, toJSON(message, false, bookendContentRunes))
	}
	end := make([]messageJSON, 0, len(last))
	for _, message := range last {
		end = append(end, toJSON(message, false, bookendContentRunes))
	}
	return start, end
}

func buildScroll(owner, profile, sessionID string, anchor int64, window int, before []memory.SearchMessage, anchorMessage *memory.SearchMessage, after []memory.SearchMessage) (string, error) {
	envelope := scrollEnvelope{Success: true, Mode: "scroll", SessionID: sessionID, AroundMessageID: anchor, Window: window, MessagesBefore: len(before), MessagesAfter: len(after), Messages: []messageJSON{}}
	for _, message := range before {
		envelope.Messages = append(envelope.Messages, toJSON(message, false, windowContentRunes))
	}
	if anchorMessage != nil {
		envelope.Messages = append(envelope.Messages, toJSON(*anchorMessage, true, windowContentRunes))
	}
	for _, message := range after {
		envelope.Messages = append(envelope.Messages, toJSON(message, false, windowContentRunes))
	}
	return marshalBounded(envelope)
}

func buildRead(owner string, summary *memory.SearchSession, sessionID string, count int, first, last []memory.SearchMessage) (string, error) {
	meta := sessionMetaJSON{When: "", Source: "unknown"}
	if summary != nil {
		meta = sessionMetaJSON{When: formatWhen(summary.StartedAt), Source: summary.Source, Model: summary.Model, Title: summary.Title}
	}
	envelope := readEnvelope{Success: true, Mode: "read", SessionID: sessionID, Link: sessionLink(owner, sessionID), SessionMeta: meta, MessageCount: count, Messages: []messageJSON{}}
	if count == 0 {
		envelope.Message = "Session has no delivered messages in this profile."
		return marshalBounded(envelope)
	}
	for _, message := range first {
		envelope.Messages = append(envelope.Messages, toJSON(message, false, messageContentRunes))
	}
	if len(last) > 0 {
		envelope.Truncated = true
		envelope.Message = "Session is larger than the read window; showing first and last messages. Pass around_message_id (any id above) to scroll the middle."
		for _, message := range last {
			envelope.Messages = append(envelope.Messages, toJSON(message, false, messageContentRunes))
		}
	}
	return marshalBounded(envelope)
}

func buildBrowse(owner string, records []memory.SessionSummaryRecord) (string, error) {
	envelope := browseEnvelope{Success: true, Mode: "browse", Count: len(records), Results: []browseResultJSON{}, Message: "Showing most recent sessions. Pass a query= to search, or session_id+around_message_id to scroll."}
	for _, record := range records {
		envelope.Results = append(envelope.Results, browseResultJSON{
			SessionID: record.SessionID, Link: sessionLink(owner, record.SessionID), Title: record.Title,
			Source: record.Source, StartedAt: toEpoch(record.StartedAt), LastActive: toEpoch(record.LastActive),
			MessageCount: record.MessageCount, Preview: clamp(record.Preview, bookendContentRunes),
		})
	}
	if len(records) == 0 {
		envelope.Message = "No delivered sessions exist for this profile yet."
	}
	return marshalBounded(envelope)
}

func toJSON(message memory.SearchMessage, anchor bool, limit int) messageJSON {
	content, truncated := clampRunes(message.Content, limit)
	rendered := messageJSON{ID: message.ID, Role: message.Role, Content: content, ToolName: message.ToolName, ToolCalls: clampToolCalls(message.ToolCalls), Timestamp: toEpoch(message.Timestamp), Anchor: anchor}
	if truncated {
		rendered.ContentTruncated = true
		rendered.OriginalContentChars = utf8.RuneCountInString(message.Content)
	}
	return rendered
}

// clampToolCalls bounds stored tool-call JSON, which is reference data only.
func clampToolCalls(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" || trimmed == "null" {
		return ""
	}
	value, _ := clampRunes(trimmed, bookendContentRunes)
	return value
}

func clamp(value string, limit int) string {
	clamped, _ := clampRunes(value, limit)
	return clamped
}

func clampRunes(value string, limit int) (string, bool) {
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	runes := []rune(value)
	return string(runes[:limit]), true
}

// marshalBounded encodes the envelope and, if it exceeds the rune budget,
// drops bookends then lower-ranked results until it fits. It never emits
// truncated JSON; the final fallback is a structurally valid stub.
func marshalBounded(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if utf8.RuneCountInString(string(encoded)) <= envelopeRunes {
		return string(encoded), nil
	}
	if envelope, ok := value.(discoverEnvelope); ok {
		for len(envelope.Results) > 1 {
			envelope.Results = envelope.Results[:len(envelope.Results)-1]
			envelope.Count = len(envelope.Results)
			last := &envelope.Results[len(envelope.Results)-1]
			last.BookendStart, last.BookendEnd = nil, nil
			encoded, err = json.Marshal(envelope)
			if err != nil {
				return "", err
			}
			if utf8.RuneCountInString(string(encoded)) <= envelopeRunes {
				return string(encoded), nil
			}
		}
	}
	stub := map[string]any{"success": true, "mode": "truncated", "message": "Session search result exceeded the size bound; narrow the query, reduce limit, or read a single session."}
	stubbed, err := json.Marshal(stub)
	if err != nil {
		return "", err
	}
	return string(stubbed), nil
}
