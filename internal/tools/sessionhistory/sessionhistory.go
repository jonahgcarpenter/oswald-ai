package sessionhistory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolctx"
)

const maxRecentCount = 3

// NewRecentHandler returns a Handler for the session.recent tool.
func NewRecentHandler(store *memory.Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		meta := toolctx.MetadataFromContext(ctx)
		if meta.SessionID == "" {
			return "", fmt.Errorf("session.recent: no session identity available in this context")
		}

		offset := intArg(args, "offset", 1)
		count := intArg(args, "count", 1)
		if offset < 1 {
			offset = 1
		}
		if count < 1 {
			count = 1
		}
		if count > maxRecentCount {
			count = maxRecentCount
		}

		turns := store.RecentTurns(meta.SessionID, offset, count)
		reqLog := log.Agent("agent.tool.session", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model)
		if len(turns) == 0 {
			reqLog.Debug("agent.tool.session.recent.empty", "no recent session exchanges found",
				config.F("tool_name", "session.recent"),
				config.F("offset", offset),
				config.F("count", count),
			)
			return "No recent session exchanges found.", nil
		}

		reqLog.Info("agent.tool.session.recent.recalled", "recalled recent session exchanges",
			config.F("tool_name", "session.recent"),
			config.F("offset", offset),
			config.F("count", count),
			config.F("returned_count", len(turns)),
		)
		return formatTurns(offset, turns), nil
	}
}

func intArg(args map[string]interface{}, key string, fallback int) int {
	value, ok := args[key]
	if !ok || value == nil {
		return fallback
	}
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

func formatTurns(offset int, turns []memory.Turn) string {
	var sb strings.Builder
	for i, turn := range turns {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "Exchange %d:\n", offset+i)
		fmt.Fprintf(&sb, "User: %s\n", strings.TrimSpace(turn.User.Content))
		fmt.Fprintf(&sb, "Assistant: %s", strings.TrimSpace(turn.Assistant.Content))
	}
	return sb.String()
}
