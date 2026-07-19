package privacyruntime

import (
	"errors"
	"sync"
	"testing"
)

func TestBusSubscribePublishAndUnsubscribe(t *testing.T) {
	bus := NewBus()
	var mu sync.Mutex
	count := 0
	unsubscribe := bus.Subscribe(func(event Event) {
		if !event.CloseConnections {
			t.Fatal("subscriber received wrong event")
		}
		mu.Lock()
		count++
		mu.Unlock()
	})
	bus.Publish(Event{CloseConnections: true})
	unsubscribe()
	unsubscribe()
	bus.Publish(Event{CloseConnections: true})
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("subscriber call count=%d want 1", count)
	}
}

func TestBusPublishesAllSubscribersAndReturnsErrors(t *testing.T) {
	bus := NewBus()
	called := 0
	bus.SubscribeError(func(Event) error {
		called++
		return errors.New("subscriber failed")
	})
	bus.Subscribe(func(Event) { called++ })
	if err := bus.Publish(Event{}); err == nil {
		t.Fatal("publish succeeded despite subscriber failure")
	}
	if called != 2 {
		t.Fatalf("subscriber calls=%d want 2", called)
	}
}
