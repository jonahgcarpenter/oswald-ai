package usermemory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/identity"
	"github.com/jonahgcarpenter/oswald-ai/internal/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/toolnames"
)

func requestLog(log *config.Logger, ctx context.Context) *config.Logger {
	meta := requestctx.MetadataFromContext(ctx)
	principal, _ := requestctx.PrincipalFromContext(ctx)
	return log.Agent("agent.tool.memory", meta.RequestID, meta.SessionID, principal.CanonicalUserID, principal.Gateway, meta.Model)
}

func authenticatedPrincipal(ctx context.Context, toolName string) (identity.Principal, error) {
	principal, _ := requestctx.PrincipalFromContext(ctx)
	if !principal.Valid() || !principal.Authenticated() {
		return identity.Principal{}, fmt.Errorf("%s: authenticated user identity is required", toolName)
	}
	return principal, nil
}

// NewSearchHandler returns a Handler for memory search.
func NewSearchHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		principal, err := authenticatedPrincipal(ctx, toolnames.UserMemorySearch)
		if err != nil {
			return "", err
		}
		userID := principal.CanonicalUserID
		limit := intArg(args, "limit", 8)
		query := stringArg(args, "query")
		if strings.TrimSpace(query) == "" {
			entries, err := store.ListMemories(userID, stringArg(args, "scope"), stringArg(args, "category"), limit)
			if err != nil {
				return "", err
			}
			if len(entries) == 0 {
				return "No matching memories found for this user.", nil
			}
			return RenderMemory("", entries), nil
		}
		results, stats := store.Recall(ctx, userID, query, RecallRequest{
			Scope: stringArg(args, "scope"), Category: stringArg(args, "category"), TopK: limit, MinRelevance: defaultRecallMinRelevance, ExplicitSearch: true,
		})
		searchLog := requestLog(log, ctx)
		if stats.LexicalError != nil {
			searchLog.Warn("agent.tool.user_memory.search_lexical_degraded", "user memory search lexical channel degraded", config.F("tool_name", toolnames.UserMemorySearch), config.F("status", "degraded"), config.ErrorField(stats.LexicalError))
		}
		if stats.SemanticError != nil {
			searchLog.Warn("agent.tool.user_memory.search_semantic_degraded", "user memory search semantic channel degraded", config.F("tool_name", toolnames.UserMemorySearch), config.F("status", "degraded"), config.ErrorField(stats.SemanticError))
		}
		if !stats.LexicalAvailable && !stats.SemanticAvailable {
			return "", fmt.Errorf("%s: retrieval indexes unavailable", toolnames.UserMemorySearch)
		}
		if len(results) == 0 {
			if stats.LexicalError != nil || stats.SemanticError != nil {
				return "No matching memories found in the available retrieval channel; recall is partially degraded.", nil
			}
			return "No matching memories found for this user.", nil
		}
		store.RecordRecallUsage(ctx, userID, results)
		searchLog.Debug("agent.tool.user_memory.searched", "searched user memory",
			config.F("tool_name", toolnames.UserMemorySearch), config.F("returned_count", len(results)),
			config.F("lexical_candidate_count", stats.LexicalCandidateCount),
			config.F("semantic_candidate_count", stats.SemanticCandidateCount))
		output := RenderDurableMemoryRecall(results, 12000)
		if stats.LexicalError != nil || stats.SemanticError != nil {
			output = "Recall is partially degraded; results come from the available retrieval channel.\n\n" + output
		}
		return output, nil
	}
}

// NewListHandler returns a Handler for listing active memory.
func NewListHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		principal, err := authenticatedPrincipal(ctx, toolnames.UserMemoryList)
		if err != nil {
			return "", err
		}
		userID := principal.CanonicalUserID
		entries, err := store.ListMemories(userID, stringArg(args, "scope"), stringArg(args, "category"), intArg(args, "limit", 25))
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			return "No active memories found for this user.", nil
		}
		intro, _ := store.ReadIntro(userID)
		requestLog(log, ctx).Debug("agent.tool.user_memory.listed", "listed user memory", config.F("tool_name", toolnames.UserMemoryList), config.F("returned_count", len(entries)))
		return RenderMemory(intro, entries), nil
	}
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
