package session_search

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/governance"
)

// attributedMessage pairs a stored row with its owning member so merged group
// views always show which user a message belongs to.
type attributedMessage struct {
	message memory.SearchMessage
	profile string
	sender  string
	scope   string
}

func senderOf(summary *memory.SearchSession) string {
	if summary == nil {
		return ""
	}
	return summary.Sender
}

func scopeOf(summary *memory.SearchSession) string {
	if summary == nil {
		return ""
	}
	return summary.Scope
}

// resolveStoreSession resolves a bare id (or conversation key) to its concrete
// session id within one store.
func resolveStoreSession(ctx context.Context, store *memory.ProfileStore, owner, id string) (string, error) {
	summary, err := store.SessionSummaryFor(ctx, owner, id)
	if err != nil {
		return "", err
	}
	if summary == nil {
		return "", nil
	}
	return summary.SessionID, nil
}

type groupMemberHandle struct {
	store   *memory.ProfileStore
	profile string
	member  memory.GroupMember
	summary *memory.SearchSession
}

// collectGroupMembers lists every stored slice of one group conversation
// across the owner and peer stores. Slices sharing the live lineage are
// dropped when liveSessionID is set: that slice is already in the model's
// active context. It returns the surviving members, a group label, and whether
// any live slice was omitted.
func collectGroupMembers(ctx context.Context, ownerStore *memory.ProfileStore, peers map[string]*memory.ProfileStore, owner, liveSessionID, source, group string) ([]groupMemberHandle, string, bool, error) {
	type namedStore struct {
		store   *memory.ProfileStore
		profile string
	}
	stores := []namedStore{{store: ownerStore, profile: owner}}
	for _, name := range orderedPeers(peers, owner) {
		if name == owner {
			continue
		}
		peer := peers[name]
		if peer == nil || peer.Profile() != name {
			continue
		}
		stores = append(stores, namedStore{store: peer, profile: name})
	}
	var members []groupMemberHandle
	var chatName string
	liveOmitted := false
	for _, candidate := range stores {
		if candidate.store == nil || candidate.store.Profile() != candidate.profile {
			continue
		}
		rows, err := candidate.store.GroupMemberSessions(ctx, candidate.profile, source, group)
		if err != nil {
			return nil, "", false, fmt.Errorf("session_search: %w", err)
		}
		for _, row := range rows {
			if liveSessionID != "" {
				hidden, err := candidate.store.HidesLive(ctx, candidate.profile, liveSessionID, row.SessionID)
				if err == nil && hidden {
					liveOmitted = true
					continue
				}
			}
			if chatName == "" && row.ChatName != "" {
				chatName = row.ChatName
			}
			summary, err := candidate.store.SessionSummaryFor(ctx, candidate.profile, row.SessionID)
			if err != nil {
				return nil, "", false, fmt.Errorf("session_search: %w", err)
			}
			members = append(members, groupMemberHandle{store: candidate.store, profile: candidate.profile, member: row, summary: summary})
		}
	}
	return members, chatName, liveOmitted, nil
}

func groupLabel(source, group, chatName string) string {
	if strings.TrimSpace(chatName) != "" {
		return chatName
	}
	return source + ":" + group
}

func membersJSON(members []groupMemberHandle) []memberJSON {
	out := make([]memberJSON, 0, len(members))
	for _, m := range members {
		count := 0
		if m.summary != nil {
			count = m.summary.MessageCount
		}
		out = append(out, memberJSON{Profile: m.profile, Sender: m.member.Sender, SessionID: m.member.SessionID, Link: sessionLink(m.profile, m.member.SessionID), MessageCount: count})
	}
	return out
}

func renderAttributed(messages []attributedMessage, limit int) []messageJSON {
	out := make([]messageJSON, 0, len(messages))
	for _, attributed := range messages {
		out = append(out, toJSON(attributed.message, false, limit, attributed.profile, attributed.sender, attributed.scope))
	}
	return out
}

func sortAttributed(messages []attributedMessage) {
	sort.SliceStable(messages, func(i, j int) bool {
		left, right := messages[i], messages[j]
		if left.message.Timestamp.Equal(right.message.Timestamp) {
			if left.profile == right.profile {
				return left.message.ID < right.message.ID
			}
			return left.profile < right.profile
		}
		return left.message.Timestamp.Before(right.message.Timestamp)
	})
}

// readGroup merges every member slice of one group conversation into a single
// chronological read. DMs never enter: membership is listed per group prefix,
// and peer DM rows live under different keys.
func readGroup(ctx context.Context, ownerStore *memory.ProfileStore, peers map[string]*memory.ProfileStore, owner, liveSessionID, sessionID, source, group string) (governance.Result, error) {
	members, chatName, liveOmitted, err := collectGroupMembers(ctx, ownerStore, peers, owner, liveSessionID, source, group)
	if err != nil {
		return governance.Result{}, err
	}
	if len(members) == 0 {
		if liveOmitted {
			return governance.Result{}, fmt.Errorf("session_search: read rejected: session is the current lineage (already in your active context)")
		}
		response, err := buildGroupRead(sessionID, groupLabel(source, group, chatName), nil, nil, 0, false, nil)
		if err != nil {
			return governance.Result{}, err
		}
		return governance.Result{Content: response, Outcome: governance.OutcomeUnproductive}, nil
	}
	var merged []attributedMessage
	var meta *memory.SearchSession
	total := 0
	for _, m := range members {
		rows, err := m.store.SessionAllMessages(ctx, m.profile, m.member.SessionID)
		if err != nil {
			return governance.Result{}, fmt.Errorf("session_search: %w", err)
		}
		for _, row := range rows {
			merged = append(merged, attributedMessage{message: row, profile: m.profile, sender: m.member.Sender, scope: m.member.Scope})
		}
		total += len(rows)
		if m.summary != nil && (meta == nil || m.summary.LastActive.After(meta.LastActive)) {
			meta = m.summary
		}
	}
	sortAttributed(merged)
	var window []messageJSON
	if total <= readHead+readTail {
		window = renderAttributed(merged, messageContentRunes)
	} else {
		window = append(window, renderAttributed(merged[:readHead], messageContentRunes)...)
		window = append(window, renderAttributed(merged[len(merged)-readTail:], messageContentRunes)...)
	}
	response, err := buildGroupRead(sessionID, groupLabel(source, group, chatName), membersJSON(members), meta, total, liveOmitted, window)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: outcomeFor(total)}, nil
}

// scrollGroup centers a merged chronological window on an anchor living in
// one member slice. The anchor session_id selects the member; the merged
// neighbors come from every member by timestamp.
func scrollGroup(ctx context.Context, ownerStore *memory.ProfileStore, peers map[string]*memory.ProfileStore, owner, liveSessionID, sessionID, source, group string, anchor sessionAnchor, anchorID int64, window int) (governance.Result, error) {
	window = max(minWindow, min(window, maxWindow))
	members, chatName, _, err := collectGroupMembers(ctx, ownerStore, peers, owner, liveSessionID, source, group)
	if err != nil {
		return governance.Result{}, err
	}
	present := false
	for _, m := range members {
		if m.profile == anchor.Profile && m.member.SessionID == anchor.SessionID {
			present = true
			break
		}
	}
	if !present {
		// The anchor slice is the live lineage: pivot on it anyway, neighbors
		// still come from the surviving members.
		summary, err := anchor.Store.SessionSummaryFor(ctx, anchor.Profile, anchor.SessionID)
		if err != nil {
			return governance.Result{}, fmt.Errorf("session_search: %w", err)
		}
		sender, scope := "", ""
		if summary != nil {
			sender, scope = summary.Sender, summary.Scope
		}
		members = append([]groupMemberHandle{{store: anchor.Store, profile: anchor.Profile, member: memory.GroupMember{SessionID: anchor.SessionID, Sender: sender, Scope: scope}, summary: summary}}, members...)
	}
	// Validate the anchor inside its own slice first so a bad id keeps the
	// single-slice error shape instead of silently pivoting.
	if _, anchorMessage, _, err := anchor.Store.SessionWindow(ctx, anchor.Profile, anchor.SessionID, anchorID, 1); err != nil {
		return governance.Result{}, fmt.Errorf("session_search: %w", err)
	} else if anchorMessage == nil {
		return governance.Result{}, fmt.Errorf("session_search: anchor message not found")
	}
	var merged []attributedMessage
	for _, m := range members {
		rows, err := m.store.SessionAllMessages(ctx, m.profile, m.member.SessionID)
		if err != nil {
			return governance.Result{}, fmt.Errorf("session_search: %w", err)
		}
		for _, row := range rows {
			merged = append(merged, attributedMessage{message: row, profile: m.profile, sender: m.member.Sender, scope: m.member.Scope})
		}
	}
	sortAttributed(merged)
	index := -1
	for i, attributed := range merged {
		if attributed.profile == anchor.Profile && attributed.message.ID == anchorID {
			index = i
			break
		}
	}
	if index < 0 {
		return governance.Result{}, fmt.Errorf("session_search: anchor message not found")
	}
	low := max(0, index-window)
	high := min(len(merged), index+window+1)
	windowed := renderAttributed(merged[low:high], windowContentRunes)
	for i := range windowed {
		if low+i == index {
			windowed[i].Anchor = true
			break
		}
	}
	response, err := buildGroupScroll(sessionID, groupLabel(source, group, chatName), membersJSON(members), anchor.Profile, anchorID, window, low, len(merged)-high, windowed)
	if err != nil {
		return governance.Result{}, err
	}
	return governance.Result{Content: response, Outcome: governance.OutcomeProductive}, nil
}
