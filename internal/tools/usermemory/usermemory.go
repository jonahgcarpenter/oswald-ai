package usermemory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/toolctx"
)

// NewHandler returns a Handler for the persistent_memory tool.
// The handler dispatches on the "action" argument:
//
//   - remember — store or update a fact (requires key and value)
//   - recall   — return the full memory profile for the current user
//   - forget   — remove a fact by key, or pass key "all" to wipe everything
//
// The target user is determined from the sender ID injected into ctx by the
// agent, so the model never needs to pass user identity as an argument.
func NewHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		userID := toolctx.SenderIDFromContext(ctx)
		if userID == "" {
			return "", fmt.Errorf("persistent_memory: no user identity available in this context")
		}

		action, _ := args["action"].(string)
		action = strings.TrimSpace(strings.ToLower(action))

		key, _ := args["key"].(string)
		key = strings.TrimSpace(key)

		value, _ := args["value"].(string)
		value = strings.TrimSpace(value)

		switch action {
		case "remember":
			return handleRemember(store, log, userID, key, value)
		case "recall":
			return handleRecall(store, log, userID)
		case "forget":
			return handleForget(store, log, userID, key)
		default:
			return "", fmt.Errorf("persistent_memory: unknown action %q — use remember, recall, or forget", action)
		}
	}
}

func handleRemember(store *Store, log *config.Logger, userID, key, value string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("persistent_memory remember: key is required")
	}
	if value == "" {
		return "", fmt.Errorf("persistent_memory remember: value is required")
	}

	if err := store.Set(userID, key, value); err != nil {
		return "", err
	}

	log.Debug("UserMemory: remembered key=%q for user=%q", key, userID)
	return fmt.Sprintf("Remembered: %s = %s", key, value), nil
}

func handleRecall(store *Store, log *config.Logger, userID string) (string, error) {
	content, err := store.Read(userID)
	if err != nil {
		return "", err
	}

	if content == "" {
		log.Debug("UserMemory: no memory found for user=%q", userID)
		return "No persistent memory found for this user.", nil
	}

	log.Debug("UserMemory: recalled memory for user=%q (%d bytes)", userID, len(content))
	return content, nil
}

func handleForget(store *Store, log *config.Logger, userID, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("persistent_memory forget: key is required (use \"all\" to wipe everything)")
	}

	if strings.ToLower(key) == "all" {
		if err := store.DeleteAll(userID); err != nil {
			return "", err
		}
		log.Debug("UserMemory: wiped all memory for user=%q", userID)
		return "All persistent memory for this user has been cleared.", nil
	}

	if err := store.Delete(userID, key); err != nil {
		return "", err
	}

	log.Debug("UserMemory: forgot key=%q for user=%q", key, userID)
	return fmt.Sprintf("Forgotten: %s", key), nil
}
