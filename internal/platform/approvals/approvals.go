// Package approvals owns pending terminal-command approvals awaiting a
// /approve or /deny chat command. Records live only in memory with a fixed
// five-minute window; expiry, restart, /new, and /stop all finalize as
// denied. No disk, no database, no configuration.
package approvals

import (
	"strings"
	"sync"
	"time"
)

// ApprovalWindow bounds how long /approve and /deny accept a pending ask.
var ApprovalWindow = 5 * time.Minute

// Decision resolves one parked ask: the waiting tool call either executes
// the approved command itself or returns a denial as its tool result.
type Decision string

const (
	// DecisionApproved unblocks the waiter to execute the command.
	DecisionApproved Decision = "approved"
	// DecisionDenied unblocks the waiter to return a denial tool result.
	DecisionDenied Decision = "denied"
)

// Pending is one escalated command awaiting a user decision. It carries the
// exact command plus its resolved execution context so the waiting tool call
// replays the identical invocation through the same guards and sandbox.
// Decide, when non-nil, signals the blocked waiter; it is shared by copy.
type Pending struct {
	RequestID  string
	Profile    string
	SessionKey string
	Command    string
	Workdir    string
	Scratch    string
	Timeout    int
	Background bool
	Persist    bool
	Created    time.Time
	Expires    time.Time
	Decide     chan Decision
}

// Notify signals a parked ask without blocking. False means no waiter is
// listening (already resolved or never parked).
func Notify(p Pending, d Decision) bool {
	if p.Decide == nil {
		return false
	}
	select {
	case p.Decide <- d:
		return true
	default:
		return false
	}
}

var (
	mu      sync.Mutex
	pending = make(map[string]*Pending)
)

func key(profile, session string) string {
	return strings.TrimSpace(profile) + "\x00" + strings.TrimSpace(session)
}

// Request parks one pending ask. False means an unexpired ask already owns
// this conversation (single-flight); the caller must not replace it.
func Request(p Pending) bool {
	k := key(p.Profile, p.SessionKey)
	if k == "\x00" {
		return false
	}
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if existing, ok := pending[k]; ok && now.Before(existing.Expires) {
		return false
	}
	p.Created = now
	p.Expires = now.Add(ApprovalWindow)
	cp := p
	pending[k] = &cp
	return true
}

// Has reports whether an unexpired ask owns this conversation. Expired
// records are swept.
func Has(profile, session string) bool {
	mu.Lock()
	defer mu.Unlock()
	existing, ok := pending[key(profile, session)]
	if !ok {
		return false
	}
	if !time.Now().Before(existing.Expires) {
		delete(pending, key(profile, session))
		Notify(*existing, DecisionDenied)
		return false
	}
	return true
}

// Take consumes the pending ask. ok=false with expired=true means the window
// lapsed (finalize as denied); ok=false with expired=false means nothing is
// waiting. An expired ask still signals its waiter as denied.
func Take(profile, session string) (Pending, bool, bool) {
	k := key(profile, session)
	mu.Lock()
	defer mu.Unlock()
	existing, ok := pending[k]
	if !ok {
		return Pending{}, false, false
	}
	delete(pending, k)
	if !time.Now().Before(existing.Expires) {
		Notify(*existing, DecisionDenied)
		return Pending{}, false, true
	}
	return *existing, true, false
}

// DenySession drops any pending ask for a conversation (for /new and /stop)
// and unblocks its waiter as denied. True means an ask was discarded.
func DenySession(profile, session string) bool {
	mu.Lock()
	defer mu.Unlock()
	k := key(profile, session)
	existing, ok := pending[k]
	if !ok {
		return false
	}
	delete(pending, k)
	Notify(*existing, DecisionDenied)
	return true
}

// Reset clears all pending asks, unblocking every waiter as denied.
// Tests only.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	for _, existing := range pending {
		Notify(*existing, DecisionDenied)
	}
	pending = make(map[string]*Pending)
}
