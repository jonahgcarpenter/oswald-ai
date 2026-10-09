package session_search

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/shared/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

const (
	defaultLimit  = 3
	maxLimit      = 10
	defaultWindow = 5
	minWindow     = 1
	maxWindow     = 20
	maxExclude    = 20
	readHead      = 20
	readTail      = 10
)

type discoveryArgs struct {
	Query         string
	Limit         int
	Sort          string
	Detail        string
	After         *time.Time
	Before        *time.Time
	Exclude       []string
	Roles         map[string]bool
	LiveSessionID string
}

// NewHandler returns the model-facing session_search handler. Each call is
// classified into exactly one of discover, scroll, read, or browse.
func NewHandler(store *memory.ProfileStore) func(context.Context, map[string]interface{}) (governance.Result, error) {
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("session_search: authenticated principal required")
		}
		if store == nil || store.Profile() != principal.CanonicalUserID {
			return governance.Result{}, errors.New("session_search: profile store unavailable")
		}
		if err := rejectUnknownFields(args); err != nil {
			return governance.Result{}, err
		}
		owner := principal.CanonicalUserID
		liveSessionID := liveSession(ctx, store, owner)
		sessionID, hasSession := stringArg(args, "session_id")
		anchor, hasAnchor := intArg(args, "around_message_id")
		_, hasQuery := stringArg(args, "query")
		if hasSession && hasAnchor {
			return scroll(ctx, store, owner, liveSessionID, sessionID, anchor, intArgDefault(args, "window", defaultWindow))
		}
		if hasSession {
			if hasQuery {
				return governance.Result{}, errors.New("session_search: use either query or session_id, not both")
			}
			return read(ctx, store, owner, liveSessionID, sessionID)
		}
		if hasAnchor {
			return governance.Result{}, errors.New("session_search: around_message_id requires session_id")
		}
		if hasQuery {
			parsed, err := parseDiscovery(args)
			if err != nil {
				return governance.Result{}, err
			}
			parsed.LiveSessionID = liveSessionID
			return discover(ctx, store, owner, parsed)
		}
		return browse(ctx, store, owner, liveSessionID, intArgDefault(args, "limit", defaultLimit))
	}
}

// liveSession resolves the active conversation's session id for exclusion.
// A missing metadata session or no active row yields "", excluding nothing.
func liveSession(ctx context.Context, store *memory.ProfileStore, owner string) string {
	meta := requestctx.MetadataFromContext(ctx)
	if meta.SessionID == "" || meta.SessionGeneration <= 0 {
		return ""
	}
	id, err := store.ActiveSessionID(ctx, owner, meta.SessionID, meta.SessionGeneration)
	if err != nil {
		return ""
	}
	return id
}

func rejectUnknownFields(args map[string]interface{}) error {
	for key := range args {
		switch key {
		case "query", "limit", "sort", "detail", "after", "before", "exclude_session_ids", "session_id", "around_message_id", "window", "role_filter":
		default:
			return fmt.Errorf("session_search: unknown argument %q", key)
		}
	}
	if raw, exists := args["role_filter"]; exists {
		if _, ok := raw.(string); !ok {
			return errors.New("session_search: role_filter must be a string")
		}
	}
	return nil
}

func parseDiscovery(args map[string]interface{}) (discoveryArgs, error) {
	parsed := discoveryArgs{Limit: defaultLimit, Sort: "", Detail: "adaptive"}
	parsed.Query, _ = stringArg(args, "query")
	parsed.Query = strings.TrimSpace(parsed.Query)
	if parsed.Query == "" {
		return parsed, errors.New("session_search: query must not be blank")
	}
	parsed.Limit = intArgDefault(args, "limit", defaultLimit)
	if parsed.Limit < 1 {
		parsed.Limit = 1
	}
	if parsed.Limit > maxLimit {
		parsed.Limit = maxLimit
	}
	if sort, ok := stringArg(args, "sort"); ok && sort != "" {
		if sort != "newest" && sort != "oldest" {
			return parsed, errors.New("session_search: sort must be newest or oldest")
		}
		parsed.Sort = sort
	}
	if detail, ok := stringArg(args, "detail"); ok && detail != "" {
		if detail != "adaptive" && detail != "full" {
			return parsed, errors.New("session_search: detail must be adaptive or full")
		}
		parsed.Detail = detail
	}
	now := time.Now().UTC()
	if after, ok := stringArg(args, "after"); ok && strings.TrimSpace(after) != "" {
		bound, err := parseBoundary(after, now, false)
		if err != nil {
			return parsed, fmt.Errorf("session_search: invalid after value")
		}
		parsed.After = &bound
	}
	if before, ok := stringArg(args, "before"); ok && strings.TrimSpace(before) != "" {
		bound, err := parseBoundary(before, now, true)
		if err != nil {
			return parsed, fmt.Errorf("session_search: invalid before value")
		}
		parsed.Before = &bound
	}
	if raw, exists := args["exclude_session_ids"]; exists {
		items, ok := raw.([]interface{})
		if !ok {
			return parsed, errors.New("session_search: exclude_session_ids must be an array")
		}
		if len(items) > maxExclude {
			return parsed, errors.New("session_search: too many exclude_session_ids")
		}
		for _, item := range items {
			value, ok := item.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return parsed, errors.New("session_search: exclude_session_ids must be nonempty strings")
			}
			parsed.Exclude = append(parsed.Exclude, value)
		}
	}
	parsed.Roles = roleSet(args, false)
	return parsed, nil
}

// roleSet parses a comma-separated role list; discovery defaults to
// user,assistant because stored tool output is usually noise.
func roleSet(args map[string]interface{}, toolOnly bool) map[string]bool {
	if raw, ok := stringArg(args, "role_filter"); ok && strings.TrimSpace(raw) != "" {
		set := map[string]bool{}
		for _, role := range strings.Split(raw, ",") {
			switch role = strings.TrimSpace(role); role {
			case "user", "assistant", "tool":
				set[role] = true
			}
		}
		if len(set) > 0 {
			return set
		}
	}
	if toolOnly {
		return map[string]bool{"tool": true}
	}
	return map[string]bool{"user": true, "assistant": true}
}

// parseBoundary interprets an ISO date/datetime or a relative duration such as
// 7d, 24h, or 2w. A before/after date-only value resolves to UTC midnight.
func parseBoundary(value string, now time.Time, before bool) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty boundary")
	}
	if duration, ok := relativeDuration(value); ok {
		if before {
			return now.Add(-duration), nil
		}
		return now.Add(-duration), nil
	}
	if len(value) <= 10 {
		if parsed, err := time.Parse("2006-01-02", value); err == nil {
			return parsed.UTC(), nil
		}
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, errors.New("unsupported boundary")
}

func relativeDuration(value string) (time.Duration, bool) {
	unit := value[len(value)-1]
	if unit != 'd' && unit != 'h' && unit != 'w' {
		return 0, false
	}
	amount, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || amount <= 0 {
		return 0, false
	}
	switch unit {
	case 'h':
		return time.Duration(amount) * time.Hour, true
	case 'd':
		return time.Duration(amount) * 24 * time.Hour, true
	default:
		return time.Duration(amount) * 7 * 24 * time.Hour, true
	}
}

func discover(ctx context.Context, store *memory.ProfileStore, owner string, parsed discoveryArgs) (governance.Result, error) {
	sessions, err := store.DiscoverySessions(ctx, owner, memory.SearchFilter{Query: parsed.Query, Limit: parsed.Limit, Sort: parsed.Sort, After: parsed.After, Before: parsed.Before, Exclude: parsed.Exclude, LiveSessionID: parsed.LiveSessionID})
	if err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	}
	response, err := buildDiscover(ctx, store, owner, parsed, sessions)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: outcomeFor(len(sessions))}, nil
}

func scroll(ctx context.Context, store *memory.ProfileStore, owner, liveSessionID, sessionID string, anchor int64, window int) (governance.Result, error) {
	if liveSessionID != "" {
		if shared, err := store.SharesLineage(ctx, owner, liveSessionID, sessionID); err == nil && shared {
			return governance.Result{}, errors.New("session_search: scroll rejected: anchor lives in the current session lineage (already in your active context)")
		}
	}
	before, anchorMessage, after, err := store.SessionWindow(ctx, owner, sessionID, anchor, window)
	if err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	}
	response, err := buildScroll(owner, store.Profile(), sessionID, anchor, window, before, anchorMessage, after)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: governance.OutcomeProductive}, nil
}

func read(ctx context.Context, store *memory.ProfileStore, owner, liveSessionID, sessionID string) (governance.Result, error) {
	if liveSessionID != "" {
		if shared, err := store.SharesLineage(ctx, owner, liveSessionID, sessionID); err == nil && shared {
			return governance.Result{}, errors.New("session_search: read rejected: session is the current lineage (already in your active context)")
		}
	}
	first, last, count, err := store.SessionTranscript(ctx, owner, sessionID, readHead, readTail)
	if err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	}
	summary, err := store.SessionSummaryFor(ctx, owner, sessionID)
	if err != nil {
		return governance.Result{}, err
	}
	response, err := buildRead(owner, summary, sessionID, count, first, last)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: outcomeFor(count)}, nil
}

func browse(ctx context.Context, store *memory.ProfileStore, owner, liveSessionID string, limit int) (governance.Result, error) {
	if limit < 1 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	records, err := store.RecentSessionsExcluding(ctx, owner, liveSessionID, limit)
	if err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	}
	response, err := buildBrowse(owner, records)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: outcomeFor(len(records))}, nil
}

func outcomeFor(count int) governance.Outcome {
	if count == 0 {
		return governance.OutcomeUnproductive
	}
	return governance.OutcomeProductive
}

func stringArg(args map[string]interface{}, key string) (string, bool) {
	value, ok := args[key].(string)
	return value, ok
}

func intArg(args map[string]interface{}, key string) (int64, bool) {
	switch value := args[key].(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case float64:
		return int64(value), true
	case float32:
		return int64(value), true
	}
	return 0, false
}

func intArgDefault(args map[string]interface{}, key string, fallback int) int {
	if value, ok := intArg(args, key); ok {
		return int(value)
	}
	return fallback
}
