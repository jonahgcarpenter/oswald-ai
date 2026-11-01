package agent

import "sync"

// Steer is one follow-up text steered into a running turn.
type Steer struct {
	Text      string
	RequestID string
}

// SteerInbox holds follow-ups steered into a running turn. The broker adds
// entries concurrently while the agent drains them at iteration boundaries.
// Drained entries are consumed into the host turn; entries still pending when
// the host finishes auto-run as the next turn.
type SteerInbox struct {
	mu      sync.Mutex
	pending []Steer
	drained []Steer
}

// NewSteerInbox creates an empty steer inbox for one running turn.
func NewSteerInbox() *SteerInbox { return &SteerInbox{} }

// Add appends one follow-up to the pending set.
func (s *SteerInbox) Add(text, requestID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pending = append(s.pending, Steer{Text: text, RequestID: requestID})
	s.mu.Unlock()
}

// Drain moves all pending follow-ups to the drained set and returns them.
func (s *SteerInbox) Drain() []Steer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	out := s.pending
	s.pending = nil
	if len(out) > 0 {
		s.drained = append(s.drained, out...)
	}
	s.mu.Unlock()
	return out
}

// Drained returns a copy of entries already consumed into the host turn.
func (s *SteerInbox) Drained() []Steer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	out := append([]Steer(nil), s.drained...)
	s.mu.Unlock()
	return out
}

// Pending returns a copy of entries not yet consumed by the host turn.
func (s *SteerInbox) Pending() []Steer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	out := append([]Steer(nil), s.pending...)
	s.mu.Unlock()
	return out
}

// PendingCount reports how many follow-ups are still waiting to be drained.
func (s *SteerInbox) PendingCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	return n
}

// Remove drops one pending entry by request ID, reporting whether it was present.
func (s *SteerInbox) Remove(requestID string) (Steer, bool) {
	if s == nil {
		return Steer{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.pending {
		if item.RequestID == requestID {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return item, true
		}
	}
	return Steer{}, false
}

// TakeAll clears pending and drained sets and returns their contents.
func (s *SteerInbox) TakeAll() (pending, drained []Steer) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	pending, drained = s.pending, s.drained
	s.pending, s.drained = nil, nil
	s.mu.Unlock()
	return pending, drained
}
