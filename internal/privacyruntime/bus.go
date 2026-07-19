// Package privacyruntime distributes transport-neutral runtime invalidations.
package privacyruntime

import (
	"errors"
	"sync"
)

// Event identifies runtime state that must be discarded after a privacy mutation.
type Event struct {
	ExternalIdentities []string
	SessionIDs         []string
	CloseConnections   bool
}

// Bus synchronously publishes privacy invalidations to runtime subscribers.
type Bus struct {
	mu          sync.RWMutex
	nextID      uint64
	subscribers map[uint64]func(Event) error
}

// NewBus creates an empty invalidation bus.
func NewBus() *Bus {
	return &Bus{subscribers: make(map[uint64]func(Event) error)}
}

// Subscribe registers a handler and returns an idempotent unsubscribe function.
func (b *Bus) Subscribe(handler func(Event)) func() {
	if b == nil || handler == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subscribers[id] = func(event Event) error {
		handler(event)
		return nil
	}
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, id)
			b.mu.Unlock()
		})
	}
}

// SubscribeError registers a handler whose failure keeps durable work retryable.
func (b *Bus) SubscribeError(handler func(Event) error) func() {
	if b == nil || handler == nil {
		return func() {}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subscribers[id] = handler
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subscribers, id)
			b.mu.Unlock()
		})
	}
}

// Publish delivers an event to the subscribers present at publication time.
func (b *Bus) Publish(event Event) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	handlers := make([]func(Event) error, 0, len(b.subscribers))
	for _, handler := range b.subscribers {
		handlers = append(handlers, handler)
	}
	b.mu.RUnlock()
	var publishErr error
	for _, handler := range handlers {
		publishErr = errors.Join(publishErr, handler(event))
	}
	return publishErr
}
