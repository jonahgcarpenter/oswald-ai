package usermemory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

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

// NewSaveHandler returns a Handler for batched grounded user-memory saves.
func NewSaveHandler(store *Store, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		principal, err := authenticatedPrincipal(ctx, toolnames.UserMemorySave)
		if err != nil {
			return "", err
		}
		userID := principal.CanonicalUserID
		meta := requestctx.MetadataFromContext(ctx)
		batch, decodeErrors, err := DecodeMemorySaveBatch(args)
		if err != nil {
			return "", fmt.Errorf("%s: %w", toolnames.UserMemorySave, err)
		}
		outcomes := store.SubmitMemorySaveBatch(ctx, userID, meta.CurrentUserText, FormationSource{
			RequestID: meta.RequestID, SessionID: meta.SessionID, Model: meta.Model,
			ExtractorVersion: FormationExtractorVersion, ToolName: toolnames.UserMemorySave,
		}, batch, nil)
		accepted, rejected := 0, len(decodeErrors)
		results := make([]memorySaveToolItemResult, 0, len(decodeErrors)+len(outcomes))
		for _, decodeErr := range decodeErrors {
			results = append(results, memorySaveToolItemResult{Index: decodeErr.InputIndex, Status: "rejected", Reason: decodeErr.Error(), Retryable: true})
		}
		for _, outcome := range outcomes {
			if outcome.Operational {
				return "", outcome.Err
			}
			if outcome.Err != nil || outcome.State != "approved" {
				rejected++
				reason := outcome.Reason
				if outcome.Err != nil {
					reason = outcome.Err.Error()
				}
				retryable := outcome.Err != nil || outcome.State == "rejected"
				status := "rejected"
				if outcome.State == "proposed" {
					status = "not_approved"
				}
				results = append(results, memorySaveToolItemResult{Index: outcome.InputIndex, Status: status, CandidateID: outcome.CandidateID, Reason: reason, Retryable: retryable})
				continue
			}
			accepted++
			results = append(results, memorySaveToolItemResult{Index: outcome.InputIndex, Status: "accepted", CandidateID: outcome.CandidateID, Reason: outcome.Reason})
		}
		requestLog(log, ctx).Debug("agent.tool.user_memory.candidates_staged", "staged user memory candidates", config.F("tool_name", toolnames.UserMemorySave), config.F("submitted_count", len(batch.Memories)+len(decodeErrors)), config.F("accepted_count", accepted), config.F("rejected_count", rejected))
		sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
		response := memorySaveToolResult{AcceptedCount: accepted, RejectedCount: rejected, Results: results}
		for _, result := range results {
			if result.Retryable {
				response.RetryGuidance = "Retry only rejected items whose arguments can be corrected using the same exact source evidence. Do not invent evidence or resubmit accepted items. Category-compatible claim_slot prefixes are identity., communication., preference./durable., project., relationship., environment., and notes."
				response.RequiredAction = "Correct and retry the retryable rejected items before producing the final answer."
				break
			}
		}
		if rejected > 0 && response.RequiredAction == "" {
			response.RetryGuidance = "Retry only rejected items whose arguments can be corrected using the same exact source evidence. Do not invent evidence or resubmit accepted items. Category-compatible claim_slot prefixes are identity., communication., preference./durable., project., relationship., environment., and notes."
			response.RequiredAction = "Do not claim rejected or not-approved items were saved."
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	}
}

type memorySaveToolResult struct {
	AcceptedCount  int                        `json:"accepted_count"`
	RejectedCount  int                        `json:"rejected_count"`
	Results        []memorySaveToolItemResult `json:"results"`
	RetryGuidance  string                     `json:"retry_guidance,omitempty"`
	RequiredAction string                     `json:"required_action,omitempty"`
}

type memorySaveToolItemResult struct {
	Index       int    `json:"index"`
	Status      string `json:"status"`
	CandidateID int64  `json:"candidate_id,omitempty"`
	Reason      string `json:"reason"`
	Retryable   bool   `json:"retryable"`
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

const defaultForgottenContentGrace = 30 * 24 * time.Hour

var (
	forgetIntentFraming = regexp.MustCompile(`(?i)^(?:please\s+)?(?:(?:can|could|would|will)\s+you\s+(?:please\s+)?|i\s+(?:want|need|would\s+like)\s+(?:you|oswald)\s+to\s+)?(forget|remove|delete)\s+(.+)$`)
	forgetIntentNegated = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|dont|never|not\s+asking\s+(?:you\s+)?to)\s+(?:forget|remove|delete)\b`)
	forgetHypothetical  = regexp.MustCompile(`(?i)\b(?:hypothetically|what\s+if|suppose|imagine|if\s+i\s+(?:asked|said|wanted)|how\s+would\s+i)\b`)
	thirdPartyMemory    = regexp.MustCompile(`(?i)\b(?:his|her|their|someone(?:\s+else)?['’]s|[a-z][a-z0-9_-]*['’]s)\s+(?:memory|memories|data|information)\b`)
	bulkForgetObject    = regexp.MustCompile(`(?i)^(?:all\b|everything\b)`)
	firstPartyObject    = regexp.MustCompile(`(?i)^(?:my\b|me\b|memory\b|memories\b|stored\b|saved\b|that\b|this\b|it\b|#?\d+\b|id\b)`)
	aboutFirstParty     = regexp.MustCompile(`(?i)\babout\s+(?:me|my)\b`)
)

// NewForgetHandler returns a Handler for deactivating one exact memory.
func NewForgetHandler(store *Store, policy config.RetentionPolicy, log *config.Logger) func(ctx context.Context, args map[string]interface{}) (string, error) {
	if policy.ForgottenContentGrace <= 0 {
		policy.ForgottenContentGrace = defaultForgottenContentGrace
	}
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		principal, err := authenticatedPrincipal(ctx, toolnames.UserMemoryForget)
		if err != nil {
			return "", err
		}
		memoryID, ok := positiveInt64Arg(args, "memory_id")
		if !ok {
			return "", fmt.Errorf("%s: memory_id must be an exact positive integer", toolnames.UserMemoryForget)
		}
		meta := requestctx.MetadataFromContext(ctx)
		if !hasExplicitForgetIntent(meta.CurrentUserText) {
			return "", fmt.Errorf("%s: current user turn does not contain an explicit first-party forget, remove, or delete request", toolnames.UserMemoryForget)
		}
		now := time.Now().UTC()
		state, err := store.ForgetMemory(ctx, principal.CanonicalUserID, memoryActorHash(principal), memoryID, meta.RequestID, now, policy)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Sprintf("No active memory with ID %d was found for the current user.", memoryID), nil
		}
		if err != nil {
			return "", err
		}
		requestLog(log, ctx).Debug("agent.tool.user_memory.forgot", "deactivated user memory", config.F("tool_name", toolnames.UserMemoryForget), config.F("memory_id", memoryID), config.F("status", state))
		return fmt.Sprintf("Memory ID %d is deactivated immediately and is no longer available to recall, lists, or profiles. Its retained canonical content is scheduled for permanent erasure after the %s grace period.", memoryID, formatGracePeriod(policy.ForgottenContentGrace)), nil
	}
}

func hasExplicitForgetIntent(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 4096 || forgetIntentNegated.MatchString(text) || forgetHypothetical.MatchString(text) || thirdPartyMemory.MatchString(text) {
		return false
	}
	if (strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`)) ||
		(strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'")) ||
		strings.HasPrefix(text, ">") {
		return false
	}
	match := forgetIntentFraming.FindStringSubmatch(text)
	if len(match) != 3 {
		return false
	}
	object := strings.TrimSpace(match[2])
	if len(object) > 512 {
		object = object[:512]
	}
	if bulkForgetObject.MatchString(object) {
		return false
	}
	return firstPartyObject.MatchString(object) || aboutFirstParty.MatchString(object)
}

func formatGracePeriod(grace time.Duration) string {
	if grace%(24*time.Hour) == 0 {
		days := int(grace / (24 * time.Hour))
		if days == 1 {
			return "1-day"
		}
		return fmt.Sprintf("%d-day", days)
	}
	return grace.String()
}

func positiveInt64Arg(args map[string]interface{}, key string) (int64, bool) {
	if args == nil {
		return 0, false
	}
	switch value := args[key].(type) {
	case int:
		return int64(value), value > 0
	case int64:
		return value, value > 0
	case float64:
		if value <= 0 || value > math.MaxInt64 || math.Trunc(value) != value {
			return 0, false
		}
		return int64(value), true
	case float32:
		converted := float64(value)
		if converted <= 0 || converted > math.MaxInt64 || math.Trunc(converted) != converted {
			return 0, false
		}
		return int64(converted), true
	default:
		return 0, false
	}
}

func memoryActorHash(principal identity.Principal) string {
	value := strings.ToLower(strings.TrimSpace(principal.Gateway)) + "\x00" + strings.ToLower(strings.TrimSpace(principal.ExternalID))
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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
