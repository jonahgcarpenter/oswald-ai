package soulmemory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// NewHandler returns a tool handler for the soul_memory tool. The handler
// gives the agent read/write/append access to its own soul file, which serves
// as its live system prompt. Changes made via write or append take effect on
// the next request without requiring a restart.
func NewHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		action, _ := args["action"].(string)
		action = strings.TrimSpace(strings.ToLower(action))

		content, _ := args["content"].(string)

		switch action {
		case "read":
			soul, err := store.Read()
			if err != nil {
				return "", fmt.Errorf("failed to read soul file: %w", err)
			}
			if soul == "" {
				return "Soul file is empty or does not exist.", nil
			}
			return soul, nil

		case "write":
			if content == "" {
				return "", fmt.Errorf("write action requires a non-empty content argument")
			}
			log.Warn("Soul file overwritten via soul_memory tool")
			if err := store.Write(content); err != nil {
				return "", fmt.Errorf("failed to write soul file: %w", err)
			}
			return "Soul file updated. Changes take effect on the next request.", nil

		case "append":
			if content == "" {
				return "", fmt.Errorf("append action requires a non-empty content argument")
			}
			log.Warn("Soul file appended via soul_memory tool")
			if err := store.Append(content); err != nil {
				return "", fmt.Errorf("failed to append to soul file: %w", err)
			}
			return "Content appended to soul file. Changes take effect on the next request.", nil

		default:
			return "", fmt.Errorf("unknown action %q — valid actions are: read, write, append", action)
		}
	}
}
