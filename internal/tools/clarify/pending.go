package clarify

import (
	"strings"
	"sync"
	"time"
)

// ClarifyWindow bounds how long a parked clarify ask waits for the user's
// answer. Expiry, /new, /stop, and restart all finalize as non-submitted.
var ClarifyWindow = 5 * time.Minute

// Answer carries one user's free-text reply to a parked clarify ask.
type Answer struct {
	Text string
}

// Pending is one parked clarify ask awaiting the user's reply. It carries
// the validated questions plus the tool call they came from so the answer
// returns as that call's tool result. Answer, when non-nil, receives the
// reply; a closed channel means the wait ended without an answer.
type Pending struct {
	RequestID  string
	Profile    string
	SessionKey string
	Questions  []Question
	ToolCallID string
	Created    time.Time
	Expires    time.Time
	Answer     chan Answer
}

func key(profile, session string) string {
	return strings.TrimSpace(profile) + "\x00" + strings.TrimSpace(session)
}

var (
	mu      sync.Mutex
	pending = make(map[string]*Pending)
)

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
	p.Expires = now.Add(ClarifyWindow)
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
		return false
	}
	return true
}

// Fulfill delivers the user's reply to the parked ask. The ask stays parked
// across sequential questions; the handler withdraws it when the wait ends.
// False means nothing is waiting (or the ask lapsed, in which case the
// message flows through as a normal turn). Expired asks never swallow a
// message: they are swept and reported as unconsumed.
func Fulfill(profile, session, text string) bool {
	k := key(profile, session)
	mu.Lock()
	defer mu.Unlock()
	existing, ok := pending[k]
	if !ok {
		return false
	}
	if !time.Now().Before(existing.Expires) {
		delete(pending, k)
		return false
	}
	if existing.Answer == nil {
		return false
	}
	select {
	case existing.Answer <- Answer{Text: text}:
	default:
	}
	return true
}

// Take consumes the pending ask. ok=false means nothing is waiting.
func Take(profile, session string) (Pending, bool) {
	k := key(profile, session)
	mu.Lock()
	defer mu.Unlock()
	existing, ok := pending[k]
	if !ok {
		return Pending{}, false
	}
	delete(pending, k)
	return *existing, true
}

// DenySession drops any pending ask for a conversation (for /new and /stop)
// and unblocks its waiter as cancelled. True means an ask was discarded.
func DenySession(profile, session string) bool {
	mu.Lock()
	defer mu.Unlock()
	k := key(profile, session)
	existing, ok := pending[k]
	if !ok {
		return false
	}
	delete(pending, k)
	if existing.Answer != nil {
		close(existing.Answer)
	}
	return true
}

// Reset clears all pending asks, unblocking every waiter as cancelled.
// Tests only.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	for _, existing := range pending {
		if existing.Answer != nil {
			close(existing.Answer)
		}
	}
	pending = make(map[string]*Pending)
}
