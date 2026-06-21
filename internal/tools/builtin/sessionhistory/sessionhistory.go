package sessionhistory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

// NewSummaryHandler returns a Handler for the session.summary tool.
func NewSummaryHandler(store *usermemory.Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		meta := requestctx.MetadataFromContext(ctx)
		if meta.SessionID == "" {
			return "", fmt.Errorf("session.summary: no session identity available in this context")
		}
		summary, err := store.ReadSessionSummary(meta.SessionID)
		if err != nil {
			return "", err
		}
		log.Agent("agent.tool.session", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model).Info("agent.tool.session.summary.recalled", "recalled session summary", config.F("tool_name", "session.summary"))
		return formatSummary(summary), nil
	}
}

func formatSummary(summary usermemory.SessionSummary) string {
	if strings.TrimSpace(summary.Summary) == "" && len(summary.OpenThreads) == 0 && len(summary.Decisions) == 0 && len(summary.UserGoals) == 0 {
		return "No session summary found."
	}
	var sb strings.Builder
	if strings.TrimSpace(summary.Summary) != "" {
		fmt.Fprintf(&sb, "Summary: %s\n", strings.TrimSpace(summary.Summary))
	}
	writeList(&sb, "Open threads", summary.OpenThreads)
	writeList(&sb, "Decisions", summary.Decisions)
	writeList(&sb, "User goals", summary.UserGoals)
	return strings.TrimSpace(sb.String())
}

func writeList(sb *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(sb, "%s:\n", label)
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			fmt.Fprintf(sb, "- %s\n", strings.TrimSpace(value))
		}
	}
}
