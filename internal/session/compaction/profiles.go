package compaction

import (
	"context"

	"github.com/jonahgcarpenter/oswald-ai/internal/session/memory"
)

// ProfileService marks delivery outcomes so reads gate on delivered rows only.
// Markdown compaction runs inline in the agent lane, so no background worker,
// lease, campaign, or low-priority gate remains.
type ProfileService struct {
	store *memory.ProfileStore
}

// NewProfileService constructs a delivery marker sharing the profile store.
func NewProfileService(store *memory.ProfileStore) *ProfileService {
	return &ProfileService{store: store}
}

// Start is a no-op retained for lifecycle symmetry; Stop joins nothing.
func (s *ProfileService) Start() {}

// Stop joins nothing; the store closes separately.
func (s *ProfileService) Stop() {}

// Enqueue acknowledges delivery after a successful gateway send.
func (s *ProfileService) Enqueue(ctx context.Context, owner string, source memory.DeliverySource) error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.MarkSessionTurnDelivered(ctx, owner, source.TurnID)
}

// MarkDeliveryFailed keeps the exchange out of context and compaction.
func (s *ProfileService) MarkDeliveryFailed(ctx context.Context, owner string, turn int64) error {
	if s == nil || s.store == nil {
		return nil
	}
	return s.store.MarkSessionTurnDeliveryFailed(ctx, owner, turn)
}
