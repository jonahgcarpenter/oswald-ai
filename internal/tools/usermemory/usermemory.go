package usermemory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/ollama"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolctx"
)

// NewHandler returns a Handler for the persistent_memory tool.
// The handler dispatches on the "action" argument:
//
//   - remember — store or update a fact (requires statement and evidence)
//   - recall   — return stored facts for the current user, optionally filtered by category
//   - forget   — remove a fact by its statement text, or pass statement "all" to wipe everything
//
// The target user is determined from the sender ID injected into ctx by the
// agent, so the model never needs to pass user identity as an argument.
//
// chatClient and model are used to perform a one-time LLM-based migration when
// recall is called on a memory file that pre-dates the category system. The LLM
// is asked to classify each flat fact into the correct category section. The
// migrated content is written back to disk so the migration only fires once.
func NewHandler(store *Store, chatClient ollama.Chatter, model string, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		userID := toolctx.SenderIDFromContext(ctx)
		if userID == "" {
			return "", fmt.Errorf("persistent_memory: no user identity available in this context")
		}

		action, _ := args["action"].(string)
		action = strings.TrimSpace(strings.ToLower(action))

		statement, _ := args["statement"].(string)
		statement = strings.TrimSpace(statement)

		evidence, _ := args["evidence"].(string)
		evidence = strings.TrimSpace(evidence)

		category, _ := args["category"].(string)
		category = strings.TrimSpace(strings.ToLower(category))

		switch action {
		case "remember":
			return handleRemember(store, log, userID, statement, evidence, category)
		case "recall":
			return handleRecall(ctx, store, chatClient, model, log, userID, category)
		case "forget":
			return handleForget(store, log, userID, statement)
		default:
			return "", fmt.Errorf("persistent_memory: unknown action %q — use remember, recall, or forget", action)
		}
	}
}

func handleRemember(store *Store, log *config.Logger, userID, statement, evidence, category string) (string, error) {
	if statement == "" {
		return "", fmt.Errorf("persistent_memory remember: statement is required")
	}
	if evidence == "" {
		return "", fmt.Errorf("persistent_memory remember: evidence is required")
	}

	if err := store.Set(userID, statement, evidence, category); err != nil {
		return "", err
	}

	cat := normalizeCategory(category)
	log.Debug("UserMemory: remembered statement for user=%q category=%q", userID, cat)
	return fmt.Sprintf("Remembered: %s (category: %s)", statement, cat), nil
}

// handleRecall returns the user's stored memory, filtered by category if provided.
// If the memory file is in the old flat format (no ## category headers), an LLM
// call is made to categorize the facts. The result is written back to disk before
// being returned so the migration only happens once.
func handleRecall(ctx context.Context, store *Store, chatClient ollama.Chatter, model string, log *config.Logger, userID, category string) (string, error) {
	// Read the raw file first to check whether migration is needed.
	raw, err := store.Read(userID)
	if err != nil {
		return "", err
	}

	if raw == "" {
		log.Debug("UserMemory: no memory found for user=%q", userID)
		return "No persistent memory found for this user.", nil
	}

	// If the file is in old flat format (no ## category headers), run the
	// LLM migration to classify each fact into the correct category.
	if needsMigration(raw) {
		log.Debug("UserMemory: old-format file detected for user=%q; running LLM migration", userID)
		migrated, migErr := migrateWithLLM(ctx, chatClient, model, raw, log)
		if migErr != nil {
			// Migration failed — return the raw content with a note so the
			// model at least sees the data and can work with it.
			log.Warn("UserMemory: LLM migration failed for user=%q: %v; returning raw content", userID, migErr)
			return "Note: Memory file could not be automatically categorized. Raw content follows:\n\n" + raw, nil
		}

		// Persist the migrated file so this only happens once.
		if writeErr := store.WriteFull(userID, migrated); writeErr != nil {
			log.Warn("UserMemory: failed to persist migrated memory for user=%q: %v", userID, writeErr)
			// Still return the migrated content even if the write failed.
		} else {
			log.Debug("UserMemory: migration complete for user=%q; file updated on disk", userID)
		}
		raw = migrated
	}

	// Category-filtered recall.
	if category != "" {
		content, err := store.ReadCategory(userID, category)
		if err != nil {
			return "", err
		}
		if content == "" {
			log.Debug("UserMemory: no memory in category=%q for user=%q", category, userID)
			return fmt.Sprintf("No stored facts in category %q for this user.", category), nil
		}
		log.Debug("UserMemory: recalled category=%q for user=%q (%d bytes)", category, userID, len(content))
		return content, nil
	}

	// Full recall — return everything.
	log.Debug("UserMemory: recalled all memory for user=%q (%d bytes)", userID, len(raw))
	return raw, nil
}

func handleForget(store *Store, log *config.Logger, userID, statement string) (string, error) {
	if statement == "" {
		return "", fmt.Errorf("persistent_memory forget: statement is required (use \"all\" to wipe everything)")
	}

	if strings.ToLower(statement) == "all" {
		if err := store.DeleteAll(userID); err != nil {
			return "", err
		}
		log.Debug("UserMemory: wiped all memory for user=%q", userID)
		return "All persistent memory for this user has been cleared.", nil
	}

	if err := store.Delete(userID, statement); err != nil {
		return "", err
	}

	log.Debug("UserMemory: forgot statement for user=%q", userID)
	return fmt.Sprintf("Forgotten: %s", statement), nil
}

// needsMigration reports whether content is in the old flat format that
// predates the category section system. A file is considered categorized if it
// contains at least one "## " heading line.
func needsMigration(content string) bool {
	if strings.Contains(content, "\n## ") {
		return false
	}
	return len(parseEntries(memoryBody(content))) > 0
}

// migrateWithLLM asks the model to classify each fact from a flat-format memory
// file into the correct category section and returns the result in the new
// categorized Markdown format. The caller is responsible for writing it to disk.
func migrateWithLLM(ctx context.Context, chatClient ollama.Chatter, model, raw string, log *config.Logger) (string, error) {
	prompt := `You are reorganizing a user memory file into categorized sections.

The four valid categories are:
- identity   — name, pronouns, age, location, occupation
- system_rules — explicit, non-negotiable instructions ("always do X", "never do Y") and corrections to AI behavior
- preferences — likes, dislikes, communication style, settings
- notes      — everything else

Below is the existing memory file content. Reorganize every fact into the correct category using this exact Markdown format. Preserve each statement line and its "- Evidence:" line exactly as written — do not paraphrase, add, or remove any facts.

Output ONLY the categorized Markdown starting with "# User Memory". No preamble, no explanation, nothing else.

Example output format:
# User Memory

## Identity

The user's name is Alex.

- Evidence: User stated "my name is Alex". Date: [2026-01-01].

## Notes

The user mentioned they enjoy hiking.

- Evidence: User said "I love hiking on weekends". Date: [2026-01-02].

Memory file to reorganize:

` + raw

	req := ollama.ChatRequest{
		Model: model,
		Messages: []ollama.ChatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}

	resp, err := chatClient.Chat(ctx, req, nil)
	if err != nil {
		return "", fmt.Errorf("LLM migration call failed: %w", err)
	}

	result := strings.TrimSpace(resp.Message.Content)
	if result == "" {
		return "", fmt.Errorf("LLM migration returned empty response")
	}

	// Validate the response looks plausible: must start with "# User Memory"
	// and contain at least one "## " category heading and one "- Evidence:" line.
	if !strings.HasPrefix(result, "# User Memory") ||
		!strings.Contains(result, "\n## ") ||
		!strings.Contains(result, "- Evidence:") {
		log.Warn("UserMemory: LLM migration response failed validation: %q", result[:min(len(result), 200)])
		return "", fmt.Errorf("LLM migration response did not match expected format")
	}

	return result, nil
}

// min returns the smaller of two ints.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
