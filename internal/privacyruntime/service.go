package privacyruntime

import (
	"context"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

const (
	defaultPollInterval = time.Second
	defaultLease        = 30 * time.Second
)

// Store is the durable invalidation outbox used by Service.
type Store interface {
	ReconcilePrivacyInvalidationLeases(context.Context, time.Time) (int64, error)
	ClaimPrivacyInvalidation(context.Context, time.Time, time.Duration) (*usermemory.PrivacyInvalidationEvent, error)
	RetryPrivacyInvalidation(context.Context, int64, time.Time, time.Time, string) error
	CompletePrivacyInvalidation(context.Context, int64, time.Time) error
}

// Service dispatches the durable privacy invalidation outbox.
type Service struct {
	store        Store
	bus          *Bus
	log          *config.Logger
	pollInterval time.Duration
	lease        time.Duration
	cancel       context.CancelFunc
	done         chan struct{}
}

// NewService creates a durable invalidation dispatcher.
func NewService(store Store, bus *Bus, log *config.Logger) *Service {
	return &Service{store: store, bus: bus, log: log, pollInterval: defaultPollInterval, lease: defaultLease}
}

// Start reconciles expired leases and starts polling.
func (s *Service) Start(parent context.Context) {
	if s == nil || s.store == nil || s.bus == nil || s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})
	_, _ = s.store.ReconcilePrivacyInvalidationLeases(ctx, time.Now().UTC())
	go s.run(ctx)
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		if err := s.DispatchOne(ctx); err != nil && s.log != nil {
			s.log.Server("privacy.dispatcher").Warn("privacy.invalidation.dispatch_failed", "failed to dispatch privacy invalidation", config.F("status", "retry"), config.ErrorField(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DispatchOne publishes at most one available event.
func (s *Service) DispatchOne(ctx context.Context) error {
	now := time.Now().UTC()
	event, err := s.store.ClaimPrivacyInvalidation(ctx, now, s.lease)
	if err != nil || event == nil {
		return err
	}
	publishErr := s.bus.Publish(Event{
		ExternalIdentities: event.ExternalIdentities,
		SessionIDs:         event.SessionIDs,
		CloseConnections:   event.CloseConnections,
	})
	if publishErr != nil {
		delay := time.Second << min(event.Attempts-1, 6)
		if retryErr := s.store.RetryPrivacyInvalidation(ctx, event.ID, now.Add(delay), now, "publish_failed"); retryErr != nil {
			return retryErr
		}
		return publishErr
	}
	return s.store.CompletePrivacyInvalidation(ctx, event.ID, time.Now().UTC())
}

// Stop stops polling without waiting for or claiming more work.
func (s *Service) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
	s.cancel = nil
}
