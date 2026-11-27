package memory

import (
	"strings"
	"time"
)

// SearchSession is one deduplicated session lineage returned by discovery.
// Sender names the human owner of the slice (session display name, falling
// back to the session key sender segment). Scope is "dm" for direct
// conversations or "group:<chat>" for group conversations. ChatName is the
// untrusted plain conversation name from the session origin.

type SearchSession struct {
	SessionID      string
	Source         string
	Model          string
	Title          string
	StartedAt      time.Time
	LastActive     time.Time
	MessageCount   int
	MatchMessageID int64
	MatchedRole    string
	Snippet        string
	Sender         string
	Scope          string
	ChatName       string
}

// SearchMessage is one stored message rendered in a window or transcript.

type SearchMessage struct {
	ID         int64
	Role       string
	Content    string
	ToolName   string
	ToolCalls  string
	ToolCallID string
	History    ToolHistory
	Timestamp  time.Time
}

// SessionSummaryRecord describes one stored session for browse results.
// Sender, Scope, and ChatName carry the same attribution as SearchSession.

type SessionSummaryRecord struct {
	SessionID    string
	Source       string
	Model        string
	Title        string
	StartedAt    time.Time
	LastActive   time.Time
	MessageCount int
	Preview      string
	Sender       string
	Scope        string
	ChatName     string
}

const (
	// searchEligible is the profile filter shared by every shape.
	// Generation equality is intentionally not required: /new keeps prior
	// sessions as searchable history, and nothing eager-deletes stored rows.
	searchEligible       = `s.profile_name=?`
	searchMessageColumns = `m.id,m.role,COALESCE(m.content,''),COALESCE(m.tool_name,''),COALESCE(m.tool_calls,''),COALESCE(m.tool_call_id,''),m.timestamp,COALESCE(v.value,'')`
	searchMessageJoin    = ` FROM messages m JOIN sessions s ON s.id=m.session_id LEFT JOIN state_meta v ON m.role='assistant' AND v.key=('oswald:v1:turn:'||m.session_id||':'||m.id) `
)

// SearchFilter bounds a discovery query.

type SearchFilter struct {
	Query   string
	Limit   int
	Sort    string
	After   *time.Time
	Before  *time.Time
	Exclude []string
	// Roles controls matching, not surrounding context. Nil defaults to user and assistant.
	Roles map[string]bool
	// LiveSessionID is the active conversation's session id; it and the
	// sessions forked from it are omitted. Older lineage generations predate
	// the live conversation and remain recallable, deduped to one entry per
	// lineage root.
	LiveSessionID string
	// GroupSource and GroupID constrain discovery to one group conversation
	// (for example one Discord channel or iMessage chat). Both must be set
	// together; when empty, no group constraint applies.
	GroupSource string
	GroupID     string
	// AllowDMs widens a group constraint to also match direct conversations.
	// The owner in a group turn sees their own DMs plus the live group;
	// peer stores always use the group-only form.
	AllowDMs bool
}

// ParseGroupSessionKey splits a group conversation session key of the form
// source:group:sender (for example discord:<channel>:<author> or
// imessage:<chatGUID>:<sender>) and reports the source and group segments.
// DM keys (source:dm:sender) and malformed keys report ok=false.

func ParseGroupSessionKey(key string) (source, groupID string, ok bool) {
	first := strings.Index(key, ":")
	last := strings.LastIndex(key, ":")
	if first < 0 || last <= first {
		return "", "", false
	}
	source, groupID, sender := key[:first], key[first+1:last], key[last+1:]
	if source != "discord" && source != "imessage" {
		return "", "", false
	}
	if groupID == "" || groupID == "dm" || sender == "" {
		return "", "", false
	}
	if strings.Contains(groupID, ":") || strings.Contains(sender, ":") {
		// Group and sender segments must not contain further separators;
		// first/last split above already isolates them, so any inner colon
		// means a malformed key rather than a nested namespace.
		if strings.Contains(groupID, ":") {
			return "", "", false
		}
	}
	return source, groupID, true
}

// SessionKeyFor returns the conversation key and source for one session id
// (or conversation key, resolved to its newest session). It reports
// sql.ErrNoRows when the session does not exist in this profile.

func senderScope(displayName, sessionKey string) (sender, scope string) {
	if source, group, ok := ParseGroupSessionKey(sessionKey); ok {
		scope = "group:" + source + ":" + group
		sender = strings.TrimSpace(displayName)
		if sender == "" {
			parts := strings.Split(sessionKey, ":")
			sender = parts[len(parts)-1]
		}
		return sender, scope
	}
	sender = strings.TrimSpace(displayName)
	if sender != "" {
		return sender, "dm"
	}
	if parts := strings.Split(sessionKey, ":"); len(parts) == 3 && parts[1] == "dm" && parts[2] != "" {
		return parts[2], "dm"
	}
	return "", "dm"
}

// SessionWindow returns an anchored slice of stored messages around anchorID.
// window is clamped to [1,20]. Only rows from this session apply.

type scanFunc func(dest ...any) error

func scanSearchMessage(scan scanFunc) (SearchMessage, error) {
	var message SearchMessage
	var timestamp float64
	var encoded string
	if err := scan(&message.ID, &message.Role, &message.Content, &message.ToolName, &message.ToolCalls, &message.ToolCallID, &timestamp, &encoded); err != nil {
		return message, err
	}
	message.Timestamp = secondsToTime(timestamp)
	if encoded != "" {
		history, err := searchExchangeHistory(encoded)
		if err != nil {
			return message, err
		}
		message.History = history
	}
	return message, nil
}

func secondsToTime(seconds float64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(seconds*1e9)).UTC()
}

// Profile returns the trusted profile this store serves.

type GroupMember struct {
	SessionID string
	Sender    string
	Scope     string
	ChatName  string
}

// GroupMemberSessions lists stored sessions in one group conversation that
// hold messages, oldest first. Empty sessions are omitted: they carry no
// shared knowledge.
