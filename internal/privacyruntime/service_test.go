package privacyruntime_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/commands/accountlinking"
	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/privacyruntime"
	"github.com/jonahgcarpenter/oswald-ai/internal/tools/builtin/usermemory"
)

func TestDispatcherRecoversCommittedEventAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	log := config.NewLogger(config.LevelError)
	store := usermemory.NewStore(path, log)
	links := accountlinking.NewService(path, store, nil, log)
	userID, err := links.EnsureAccount("websocket", "external", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveSessionProfile(ctx, userID, "session", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSessionPrivacy(ctx, userID, strings.Repeat("a", 64), "session", "operation", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := links.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := usermemory.NewStore(path, log)
	defer reopened.Close() // nolint:errcheck
	bus := privacyruntime.NewBus()
	deliveries := 0
	bus.Subscribe(func(event privacyruntime.Event) {
		deliveries++
		if len(event.ExternalIdentities) != 1 || len(event.SessionIDs) != 1 {
			t.Fatalf("event scope=%+v", event)
		}
	})
	dispatcher := privacyruntime.NewService(reopened, bus, log)
	if err := dispatcher.DispatchOne(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchOne(ctx); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 {
		t.Fatalf("deliveries=%d want 1", deliveries)
	}
}

type retryStore struct {
	event       usermemory.PrivacyInvalidationEvent
	retryCalled bool
}

func (s *retryStore) ReconcilePrivacyInvalidationLeases(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (s *retryStore) ClaimPrivacyInvalidation(context.Context, time.Time, time.Duration) (*usermemory.PrivacyInvalidationEvent, error) {
	if s.retryCalled {
		return nil, nil
	}
	return &s.event, nil
}
func (s *retryStore) RetryPrivacyInvalidation(_ context.Context, id int64, _, _ time.Time, code string) error {
	if id != s.event.ID || code != "publish_failed" {
		return errors.New("wrong retry metadata")
	}
	s.retryCalled = true
	return nil
}
func (*retryStore) CompletePrivacyInvalidation(context.Context, int64, time.Time) error {
	return errors.New("completed failed publication")
}

func TestDispatcherRetriesSubscriberFailure(t *testing.T) {
	store := &retryStore{event: usermemory.PrivacyInvalidationEvent{ID: 7, Attempts: 1}}
	bus := privacyruntime.NewBus()
	bus.SubscribeError(func(privacyruntime.Event) error { return errors.New("offline") })
	dispatcher := privacyruntime.NewService(store, bus, config.NewLogger(config.LevelError))
	if err := dispatcher.DispatchOne(context.Background()); err == nil {
		t.Fatal("dispatch succeeded")
	}
	if !store.retryCalled {
		t.Fatal("publish failure was not released for retry")
	}
}
