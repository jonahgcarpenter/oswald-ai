package session_search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/requestctx"
	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

const (
	discoveryDefaultLimit = 5
	browseDefaultLimit    = 3
	maxLimit              = 10
	defaultWindow         = 5
	minWindow             = 1
	maxWindow             = 20
	maxExclude            = 20
	readHead              = 20
	readTail              = 10
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
// classified into exactly one of discover, scroll, read, or browse. Peers maps
// profile names to their stores and enables group-scoped cross-profile reads
// without a profile argument: discovery fans out to peer profiles whose
// sessions share the live group conversation, and a bare session_id reading a
// group conversation merges every member slice chronologically with per-message
// profile/sender attribution. DMs stay owner-only. The map is read at request
// time, so a shared map populated during startup is safe to pass before it is
// complete.
func NewHandler(store *memory.ProfileStore, peerMaps ...map[string]*memory.ProfileStore) func(context.Context, map[string]interface{}) (governance.Result, error) {
	peers := map[string]*memory.ProfileStore{}
	for _, m := range peerMaps {
		for name, peer := range m {
			if peer != nil {
				peers[name] = peer
			}
		}
	}
	return func(ctx context.Context, args map[string]interface{}) (governance.Result, error) {
		if err := ctx.Err(); err != nil {
			return governance.Result{}, err
		}
		principal, ok := requestctx.PrincipalFromContext(ctx)
		if !ok || !principal.Authenticated() {
			return governance.Result{}, errors.New("session_search: authenticated principal required")
		}
		owner := principal.CanonicalUserID
		ownerStore := peers[owner]
		if ownerStore == nil {
			ownerStore = store
		}
		if ownerStore == nil || ownerStore.Profile() != owner {
			return governance.Result{}, errors.New("session_search: profile store unavailable")
		}
		if err := rejectUnknownFields(args); err != nil {
			return governance.Result{}, err
		}
		liveSessionID, err := liveSession(ctx, ownerStore, owner)
		if err != nil {
			return governance.Result{}, fmt.Errorf("session_search: resolve live session: %w", err)
		}
		sessionID, hasSession := stringArg(args, "session_id")
		anchor, hasAnchor := intArg(args, "around_message_id")
		_, hasQuery := stringArg(args, "query")
		if hasSession && hasAnchor {
			return scroll(ctx, ownerStore, peers, owner, liveSessionID, sessionID, anchor, intArgDefault(args, "window", defaultWindow))
		}
		if hasSession {
			if hasQuery {
				return governance.Result{}, errors.New("session_search: use either query or session_id, not both")
			}
			return read(ctx, ownerStore, peers, owner, liveSessionID, sessionID)
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
			return discover(ctx, ownerStore, peers, owner, parsed)
		}
		return browse(ctx, ownerStore, owner, liveSessionID, intArgDefault(args, "limit", browseDefaultLimit))
	}
}

// liveSession resolves the active conversation's session id for exclusion.
// A missing metadata session or no active row yields "", excluding nothing.
func liveSession(ctx context.Context, store *memory.ProfileStore, owner string) (string, error) {
	meta := requestctx.MetadataFromContext(ctx)
	if meta.SessionID == "" || meta.SessionGeneration <= 0 {
		return "", nil
	}
	return store.ActiveSessionID(ctx, owner, meta.SessionID, meta.SessionGeneration)
}

func rejectUnknownFields(args map[string]interface{}) error {
	for key := range args {
		switch key {
		case "query", "sort", "detail", "after", "before", "session_id", "role_filter":
			if _, ok := args[key].(string); !ok {
				return fmt.Errorf("session_search: %s must be a string", key)
			}
		case "limit", "around_message_id", "window":
			if _, ok := intArg(args, key); !ok {
				return fmt.Errorf("session_search: %s must be an integer", key)
			}
		case "exclude_session_ids":
			if _, ok := args[key].([]interface{}); !ok {
				return errors.New("session_search: exclude_session_ids must be an array")
			}
		default:
			return fmt.Errorf("session_search: unknown argument %q", key)
		}
	}
	if value, ok := stringArg(args, "session_id"); ok && strings.TrimSpace(value) == "" {
		return errors.New("session_search: session_id must not be blank")
	}
	if raw, exists := stringArg(args, "role_filter"); exists {
		for _, role := range strings.Split(raw, ",") {
			switch strings.TrimSpace(role) {
			case "user", "assistant", "tool":
			default:
				return errors.New("session_search: role_filter must contain user, assistant, or tool")
			}
		}
	}
	return nil
}

func parseDiscovery(args map[string]interface{}) (discoveryArgs, error) {
	parsed := discoveryArgs{Limit: discoveryDefaultLimit, Sort: "", Detail: "adaptive"}
	parsed.Query, _ = stringArg(args, "query")
	parsed.Query = strings.TrimSpace(parsed.Query)
	if parsed.Query == "" {
		return parsed, errors.New("session_search: query must not be blank")
	}
	parsed.Limit = intArgDefault(args, "limit", discoveryDefaultLimit)
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
		bound, err := parseBoundary(after, now)
		if err != nil {
			return parsed, fmt.Errorf("session_search: invalid after value")
		}
		parsed.After = &bound
	}
	if before, ok := stringArg(args, "before"); ok && strings.TrimSpace(before) != "" {
		bound, err := parseBoundary(before, now)
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
	parsed.Roles = roleSet(args)
	if parsed.After != nil && parsed.Before != nil && !parsed.After.Before(*parsed.Before) {
		return parsed, errors.New("session_search: after must precede before")
	}
	return parsed, nil
}

// roleSet parses a comma-separated role list; discovery defaults to
// user,assistant because stored tool output is usually noise.
func roleSet(args map[string]interface{}) map[string]bool {
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
	return map[string]bool{"user": true, "assistant": true}
}

// parseBoundary interprets an ISO date/datetime or a relative duration such as
// 7d, 24h, or 2w. A before/after date-only value resolves to UTC midnight.
func parseBoundary(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty boundary")
	}
	if duration, ok := relativeDuration(value); ok {
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
	factor := time.Hour
	if unit == 'd' {
		factor *= 24
	} else if unit == 'w' {
		factor *= 7 * 24
	}
	if int64(amount) > math.MaxInt64/int64(factor) {
		return 0, false
	}
	return time.Duration(amount) * factor, true
}

func discover(ctx context.Context, ownerStore *memory.ProfileStore, peers map[string]*memory.ProfileStore, owner string, parsed discoveryArgs) (governance.Result, error) {
	hits := []discoverHit{}
	scopeSource, scopeGroup, inGroup := liveGroupScope(ctx, ownerStore, owner, parsed.LiveSessionID)
	ownFilter := memory.SearchFilter{Query: parsed.Query, Limit: parsed.Limit, Sort: parsed.Sort, After: parsed.After, Before: parsed.Before, Exclude: parsed.Exclude, Roles: parsed.Roles, LiveSessionID: parsed.LiveSessionID}
	if inGroup {
		// Owner in a group turn sees their own DMs plus the live group.
		ownFilter.GroupSource, ownFilter.GroupID, ownFilter.AllowDMs = scopeSource, scopeGroup, true
	}
	own, err := ownerStore.DiscoverySessions(ctx, owner, ownFilter)
	if err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	}
	for _, session := range own {
		hits = append(hits, discoverHit{Profile: owner, Store: ownerStore, Session: session})
	}
	if inGroup {
		for _, name := range orderedPeers(peers, owner) {
			if name == owner {
				continue
			}
			peer := peers[name]
			if peer == nil || peer.Profile() != name {
				continue
			}
			sessions, err := peer.DiscoverySessions(ctx, name, memory.SearchFilter{Query: parsed.Query, Limit: parsed.Limit, Sort: parsed.Sort, After: parsed.After, Before: parsed.Before, Roles: parsed.Roles, GroupSource: scopeSource, GroupID: scopeGroup})
			if err != nil {
				continue
			}
			for _, session := range sessions {
				hits = append(hits, discoverHit{Profile: name, Store: peer, Session: session})
			}
		}
	}
	if len(hits) > parsed.Limit {
		hits = hits[:parsed.Limit]
	}
	response, err := buildDiscover(ctx, parsed, hits)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: outcomeFor(len(hits))}, nil
}

// orderedPeers lists known peer profiles with the owner first, then the rest
// in sorted order. The owner-first order makes bare session_id resolution
// prefer the caller's own slice when an id exists in several stores.
func orderedPeers(peers map[string]*memory.ProfileStore, owner string) []string {
	names := make([]string, 0, len(peers)+1)
	seen := map[string]bool{}
	if _, ok := peers[owner]; ok {
		names = append(names, owner)
		seen[owner] = true
	}
	rest := make([]string, 0, len(peers))
	for name := range peers {
		if name != owner && !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(names, rest...)
}

// liveGroupScope derives the group conversation (source plus channel/chat id)
// bounding cross-profile reads. Request metadata is preferred so the first
// group turn can already recall peers; the live session row is the fallback.
func liveGroupScope(ctx context.Context, ownerStore *memory.ProfileStore, owner, liveSessionID string) (source, group string, ok bool) {
	meta := requestctx.MetadataFromContext(ctx)
	if (meta.GroupGateway == "discord" || meta.GroupGateway == "imessage") && strings.TrimSpace(meta.GroupChatID) != "" {
		if key := strings.TrimSpace(meta.SessionID); key != "" {
			parsedSource, parsedGroup, parsed := memory.ParseGroupSessionKey(key)
			if !parsed || parsedSource != meta.GroupGateway || parsedGroup != strings.TrimSpace(meta.GroupChatID) {
				return "", "", false
			}
		}
		return meta.GroupGateway, strings.TrimSpace(meta.GroupChatID), true
	}
	if strings.TrimSpace(liveSessionID) == "" {
		return "", "", false
	}
	key, _, err := ownerStore.SessionKeyFor(ctx, owner, liveSessionID)
	if err != nil {
		return "", "", false
	}
	source, group, ok = memory.ParseGroupSessionKey(key)
	if !ok {
		return "", "", false
	}
	return source, group, true
}

// sessionAnchor is one store holding a copy of a bare session_id. Group
// conversations keep one session row per sender, so a group id resolves in
// every member profile while a DM id resolves only in its owner.
type sessionAnchor struct {
	Store     *memory.ProfileStore
	Profile   string
	SessionID string
	Key       string
}

// findSessionStores resolves a bare session_id across the owner and peer
// stores, owner first. Ids are opaque to the model; the group anchor below
// decides which matches may be read.
func findSessionStores(ctx context.Context, ownerStore *memory.ProfileStore, peers map[string]*memory.ProfileStore, owner, sessionID string) []sessionAnchor {
	stores := map[string]*memory.ProfileStore{}
	for _, name := range orderedPeers(peers, owner) {
		if peers[name] != nil && peers[name].Profile() == name {
			stores[name] = peers[name]
		}
	}
	if ownerStore != nil && ownerStore.Profile() == owner {
		stores[owner] = ownerStore
	}
	var matches []sessionAnchor
	for _, name := range orderedPeers(stores, owner) {
		store := stores[name]
		if store == nil {
			continue
		}
		key, _, err := store.SessionKeyFor(ctx, name, sessionID)
		if err != nil || strings.TrimSpace(key) == "" {
			continue
		}
		resolved := sessionID
		if id, err := resolveStoreSession(ctx, store, name, sessionID); err == nil && id != "" {
			resolved = id
		}
		matches = append(matches, sessionAnchor{Store: store, Profile: name, SessionID: resolved, Key: key})
	}
	return matches
}

// checkGroupRead verifies a group anchor belongs to the live group
// conversation. DM keys and other groups fail closed without revealing which
// sessions exist elsewhere.
func checkGroupRead(ctx context.Context, ownerStore *memory.ProfileStore, owner, liveSessionID, key string) (source, group string, err error) {
	source, group, parsed := memory.ParseGroupSessionKey(key)
	if !parsed {
		return "", "", errors.New("session_search: session not found in this group")
	}
	scopeSource, scopeGroup, ok := liveGroupScope(ctx, ownerStore, owner, liveSessionID)
	if !ok || scopeSource != source || scopeGroup != group {
		return "", "", errors.New("session_search: session not found in this group")
	}
	return source, group, nil
}

func scroll(ctx context.Context, ownerStore *memory.ProfileStore, peers map[string]*memory.ProfileStore, owner, liveSessionID, sessionID string, anchor int64, window int) (governance.Result, error) {
	matches := findSessionStores(ctx, ownerStore, peers, owner, sessionID)
	if len(matches) == 0 {
		return governance.Result{}, errors.New("session_search: session not found")
	}
	target := matches[0]
	if source, group, ok := parseGroupKey(target.Key); ok {
		if _, _, err := checkGroupRead(ctx, ownerStore, owner, liveSessionID, target.Key); err != nil {
			return governance.Result{}, err
		}
		return scrollGroup(ctx, ownerStore, peers, owner, liveSessionID, sessionID, source, group, target, anchor, window)
	}
	if target.Profile != owner {
		return governance.Result{}, errors.New("session_search: session not found in this group")
	}
	if liveSessionID != "" {
		shared, err := target.Store.SharesLineage(ctx, target.Profile, liveSessionID, target.SessionID)
		if err != nil {
			return governance.Result{}, fmt.Errorf("session_search: resolve session lineage: %w", err)
		}
		if shared {
			return governance.Result{}, errors.New("session_search: scroll rejected: anchor lives in the current session lineage (already in your active context)")
		}
	}
	before, anchorMessage, after, err := target.Store.SessionWindow(ctx, target.Profile, target.SessionID, anchor, window)
	if err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	}
	summary, err := target.Store.SessionSummaryFor(ctx, target.Profile, target.SessionID)
	if err != nil {
		return governance.Result{}, err
	}
	response, err := buildScroll(ctx, target.Store, target.Profile, target.Profile, senderOf(summary), scopeOf(summary), target.SessionID, anchor, max(minWindow, min(window, maxWindow)), before, anchorMessage, after)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: governance.OutcomeProductive}, nil
}

func read(ctx context.Context, ownerStore *memory.ProfileStore, peers map[string]*memory.ProfileStore, owner, liveSessionID, sessionID string) (governance.Result, error) {
	matches := findSessionStores(ctx, ownerStore, peers, owner, sessionID)
	if len(matches) == 0 {
		return governance.Result{}, errors.New("session_search: session not found")
	}
	target := matches[0]
	if source, group, ok := parseGroupKey(target.Key); ok {
		if _, _, err := checkGroupRead(ctx, ownerStore, owner, liveSessionID, target.Key); err != nil {
			return governance.Result{}, err
		}
		return readGroup(ctx, ownerStore, peers, owner, liveSessionID, sessionID, source, group)
	}
	if target.Profile != owner {
		return governance.Result{}, errors.New("session_search: session not found in this group")
	}
	if liveSessionID != "" {
		shared, err := target.Store.SharesLineage(ctx, target.Profile, liveSessionID, target.SessionID)
		if err != nil {
			return governance.Result{}, fmt.Errorf("session_search: resolve session lineage: %w", err)
		}
		if shared {
			return governance.Result{}, errors.New("session_search: read rejected: session is the current lineage (already in your active context)")
		}
	}
	first, last, count, err := target.Store.SessionTranscript(ctx, target.Profile, target.SessionID, readHead, readTail)
	if err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	}
	summary, err := target.Store.SessionSummaryFor(ctx, target.Profile, target.SessionID)
	if err != nil {
		return governance.Result{}, err
	}
	response, err := buildRead(target.Profile, senderOf(summary), scopeOf(summary), summary, target.SessionID, count, first, last)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: outcomeFor(count)}, nil
}

// parseGroupKey splits a stored session key into its group coordinates.
func parseGroupKey(key string) (source, group string, ok bool) {
	return memory.ParseGroupSessionKey(key)
}

func browse(ctx context.Context, ownerStore *memory.ProfileStore, owner, liveSessionID string, limit int) (governance.Result, error) {
	if limit < 1 {
		limit = browseDefaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	scopeSource, scopeGroup, inGroup := liveGroupScope(ctx, ownerStore, owner, liveSessionID)
	var groupSource, groupID string
	if inGroup {
		groupSource, groupID = scopeSource, scopeGroup
	}
	records, err := ownerStore.RecentSessionsExcluding(ctx, owner, liveSessionID, limit, groupSource, groupID)
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
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value >= float64(math.MaxInt64) || value < float64(math.MinInt64) {
			return 0, false
		}
		return int64(value), true
	case float32:
		return intArg(map[string]interface{}{key: float64(value)}, key)
	}
	return 0, false
}

func intArgDefault(args map[string]interface{}, key string, fallback int) int {
	if value, ok := intArg(args, key); ok {
		return int(value)
	}
	return fallback
}
