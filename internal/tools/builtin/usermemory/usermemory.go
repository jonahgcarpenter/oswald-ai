package usermemory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
)

func requestLog(log *config.Logger, ctx context.Context) *config.Logger {
	meta := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	return log.Agent("agent.tool.memory", meta.RequestID, meta.SessionID, principal.CanonicalUserID, principal.Gateway, meta.Model)
}

func canonicalUserID(ctx context.Context) string {
	principal, _ := requestctx.PrincipalFromContext(ctx)
	if !principal.Valid() {
		return ""
	}
	return principal.CanonicalUserID
}

// NewSaveHandler returns a Handler for explicit user-requested memory saves.
func NewSaveHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		userID := canonicalUserID(ctx)
		if userID == "" {
			return "", fmt.Errorf("memory.save: no user identity available in this context")
		}
		meta := requestctx.MetadataFromContext(ctx)
		statement := stringArg(args, "statement")
		if statement == "" {
			return "", fmt.Errorf("memory.save: statement is required")
		}
		evidence := stringArg(args, "evidence")
		if evidence == "" {
			evidence = "The user explicitly asked this to be remembered"
		}
		ttlDays := intArg(args, "ttl_days", 0)
		ttl := time.Duration(0)
		if ttlDays > 0 {
			ttl = time.Duration(ttlDays) * 24 * time.Hour
		}
		entry, err := store.SaveMemory(ctx, userID, SaveRequest{
			Scope:           stringArg(args, "scope"),
			Category:        stringArg(args, "category"),
			Statement:       statement,
			Evidence:        evidence,
			Confidence:      floatArg(args, "confidence", 0.9),
			Importance:      intArg(args, "importance", 3),
			SourceSessionID: meta.SessionID,
			TTL:             ttl,
			Supersedes:      stringArg(args, "supersedes"),
		})
		if err != nil {
			return "", err
		}
		requestLog(log, ctx).Debug("agent.tool.memory.saved", "saved memory", config.F("tool_name", "memory.save"), config.F("scope", entry.Scope), config.F("category", entry.Category))
		return fmt.Sprintf("Saved %s memory (%s): %s", entry.Scope, entry.Category, entry.Statement), nil
	}
}

// NewSearchHandler returns a Handler for memory search.
func NewSearchHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		userID := canonicalUserID(ctx)
		if userID == "" {
			return "", fmt.Errorf("memory.search: no user identity available in this context")
		}
		entries, err := store.Search(ctx, userID, stringArg(args, "scope"), stringArg(args, "category"), stringArg(args, "query"), intArg(args, "limit", 8))
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return "No matching memories found for this user.", nil
		}
		requestLog(log, ctx).Debug("agent.tool.memory.searched", "searched memory", config.F("tool_name", "memory.search"), config.F("returned_count", len(entries)))
		return RenderMemory("", entries), nil
	}
}

// NewListHandler returns a Handler for listing active memory.
func NewListHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		userID := canonicalUserID(ctx)
		if userID == "" {
			return "", fmt.Errorf("memory.list: no user identity available in this context")
		}
		entries, err := store.ListMemories(userID, stringArg(args, "scope"), stringArg(args, "category"), intArg(args, "limit", 25))
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return "No active memories found for this user.", nil
		}
		intro, _ := store.ReadIntro(userID)
		requestLog(log, ctx).Debug("agent.tool.memory.listed", "listed memory", config.F("tool_name", "memory.list"), config.F("returned_count", len(entries)))
		return RenderMemory(intro, entries), nil
	}
}

// NewForgetHandler returns a Handler for deleting active memories.
func NewForgetHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		userID := canonicalUserID(ctx)
		if userID == "" {
			return "", fmt.Errorf("memory.forget: no user identity available in this context")
		}
		target := stringArg(args, "statement")
		if target == "" {
			target = stringArg(args, "target")
		}
		if target == "" {
			return "", fmt.Errorf("memory.forget: statement or target is required")
		}
		count, err := store.Forget(userID, target, stringArg(args, "scope"))
		if err != nil {
			return "", err
		}
		requestLog(log, ctx).Debug("agent.tool.memory.forgot", "forgot memory", config.F("tool_name", "memory.forget"), config.F("deleted_count", count))
		if count == 0 {
			return "No matching active memories were found.", nil
		}
		return fmt.Sprintf("Deleted %d matching memory entry(s).", count), nil
	}
}

// Backward-compatible wrappers for old internal tests/callers.
func NewRememberHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return NewSaveHandler(store, log)
}

func NewRecallHandler(store *Store, _ interface{}, _ string, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return NewSearchHandler(store, log)
}

func stringArg(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func intArg(args map[string]interface{}, key string, fallback int) int {
	if args == nil || args[key] == nil {
		return fallback
	}
	switch v := args[key].(type) {
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

func floatArg(args map[string]interface{}, key string, fallback float64) float64 {
	if args == nil || args[key] == nil {
		return fallback
	}
	switch v := args[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return fallback
}
