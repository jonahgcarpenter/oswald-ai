package memory

import (
	"context"
)

// RecentCompletedExchangesAfter returns newest complete delivered exchanges.
func (s *ProfileStore) RecentCompletedExchangesAfter(ctx context.Context, owner, key string, generation int, after int64, limit int) ([]SessionTurn, error) {
	return s.deliveredProfileTurns(ctx, owner, key, generation, after, limit, true)
}

// profileSummaryKey addresses the legacy durable summary row. No new summaries
// are written; the key remains only so expiry sweeps and late-delivery repair
// can retire orphaned rows.
func profileSummaryKey(id string) string { return "oswald:v1:summary:" + id }
