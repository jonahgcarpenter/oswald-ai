// Package session_search exposes delivered past conversation history to the
// model through bounded, database-backed search and read shapes.
package session_search

import "github.com/jonahgcarpenter/oswald-ai/internal/llm"

// Name is the model-facing past-conversation recall tool name.
const Name = "session_search"

// Definition returns the session_search tool's model-facing description and schema.
func Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        Name,
		Description: "Recall past conversations: search or read old Oswald sessions (full-text indexed), or scroll inside one. Four shapes, picked by args: `query` = discovery (top matching sessions, top result fully hydrated); `session_id` + `around_message_id` = scroll (window of messages around an anchor); `session_id` alone = read a whole session - how you resolve an `@session:<profile>/<id>` link (split on '/' into profile + id); no args = browse recent sessions. Results are actual stored messages, no model summarization. Searches conversation history ONLY - when the user gave a direct source (URL, file, contact, live system), inspect that first; never conclude 'not found' from history alone. Use for questions about past conversations: 'what did we do about X', 'where did we leave Y'. When referring the user to a session, write its `link` value verbatim inline (it renders as a titled link).",
		Parameters: llm.ToolParameters{Type: "object", Properties: map[string]llm.ToolParameterProperty{
			"query": {
				Type:        "string",
				Description: "Search query (discovery shape). Keywords, phrases, or boolean expressions to find in past sessions. Omit to browse recent sessions. Ignored when session_id + around_message_id are set (scroll shape).",
			},
			"limit": {
				Type:        "integer",
				Description: "Discovery and browse shapes. Max sessions to return (default 3, max 10). Bump to 5-10 when the topic likely spans several sessions and you want to pick the right one to scroll into.",
			},
			"sort": {
				Type:        "string",
				Enum:        []string{"newest", "oldest"},
				Description: "Discovery shape only. Temporal bias on top of full-text ranking: omit for relevance-only (exploratory recall), 'newest' for \"where did we leave X\", 'oldest' for \"how did X start\".",
			},
			"detail": {
				Type:        "string",
				Enum:        []string{"adaptive", "full"},
				Description: "Discovery shape only. 'adaptive' (default) fully hydrates the top-ranked result and returns only the exact anchor message for lower-ranked results. 'full' returns bookends and the complete anchored window for every result.",
			},
			"after": {
				Type:        "string",
				Description: "Discovery shape only. Inclusive lower bound on session start time. ISO date/datetime (e.g. 2026-06-01) or relative duration (7d, 24h, 2w = within the last N). Use only when the user names a time frame. sort is a ranking bias, not a bound.",
			},
			"before": {
				Type:        "string",
				Description: "Discovery shape only. Exclusive upper bound on session start time. ISO date/datetime (a date-only value is midnight UTC that day) or relative duration (7d = older than a week). Use only when the user names a time frame.",
			},
			"exclude_session_ids": {
				Type:        "array",
				Description: "Discovery shape only. Session ids already inspected this task. Those sessions and their lineage are omitted so a later query explores instead of repeating the same hit. Cap 20.",
				Items:       &llm.ToolParameterProperty{Type: "string"},
			},
			"session_id": {
				Type:        "string",
				Description: "Session to read, using the session_id returned from discovery or browse, or the id segment of an @session link. Alone reads the session; pair with around_message_id to scroll. Only the authenticated profile is accessible.",
			},
			"around_message_id": {
				Type:        "integer",
				Description: "Scroll shape. Message id to center the window on - use match_message_id from a discovery result, or any id from a prior window.",
			},
			"window": {
				Type:        "integer",
				Description: "Scroll shape only. Messages to return on each side of the anchor (anchor itself always included). Clamped to [1, 20]. Default 5.",
			},
			"role_filter": {
				Type:        "string",
				Description: "Discovery match roles only, not surrounding context. Comma-separated roles, default 'user,assistant'. Pass 'user,assistant,tool' or 'tool' to match searchable stored tool output. Read and scroll also include bounded native tool traces, without image bytes or reasoning.",
			},
		}},
	}
}
