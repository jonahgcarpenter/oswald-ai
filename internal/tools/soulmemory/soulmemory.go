package soulmemory

import (
	"context"
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolctx"
)

func requestLog(log *config.Logger, ctx context.Context) *config.Logger {
	meta := toolctx.MetadataFromContext(ctx)
	return log.Agent("agent.tool.soul", meta.RequestID, meta.SessionID, meta.SenderID, meta.Gateway, meta.Model)
}

// NewReadHandler returns a tool handler for the soul.read tool.
func NewReadHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		soul, err := store.Read()
		if err != nil {
			return "", fmt.Errorf("failed to read soul file: %w", err)
		}
		if soul == "" {
			return "Soul file is empty or does not exist.", nil
		}
		return soul, nil
	}
}

// NewWriteHandler returns a tool handler for the soul.write tool.
func NewWriteHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		content, _ := args["content"].(string)
		if content == "" {
			return "", fmt.Errorf("soul.write: content is required")
		}

		reqLog := requestLog(log, ctx)
		reqLog.Warn("agent.tool.soul.overwritten", "overwrote soul memory", config.F("tool_name", "soul.write"), config.F("action", "write"))
		if err := store.Write(content); err != nil {
			return "", fmt.Errorf("failed to write soul file: %w", err)
		}
		return "Soul file updated. Changes take effect on the next request.", nil
	}
}

// NewAppendHandler returns a tool handler for the soul.append tool.
func NewAppendHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		content, _ := args["content"].(string)
		if content == "" {
			return "", fmt.Errorf("soul.append: content is required")
		}

		reqLog := requestLog(log, ctx)
		reqLog.Warn("agent.tool.soul.appended", "appended soul memory", config.F("tool_name", "soul.append"), config.F("action", "append"))
		if err := store.Append(content); err != nil {
			return "", fmt.Errorf("failed to append to soul file: %w", err)
		}
		return "Content appended to soul file. Changes take effect on the next request.", nil
	}
}
