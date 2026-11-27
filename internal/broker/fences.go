package broker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// CancelReport describes foreground agent work affected by a cancellation request.

type userFence struct {
	mu             sync.Mutex
	cond           *sync.Cond
	readers        int
	writer         bool
	pendingWriters int
}

func newUserFence() *userFence {
	fence := &userFence{}
	fence.cond = sync.NewCond(&fence.mu)
	return fence
}

func (f *userFence) reserveWriter() {
	f.mu.Lock()
	f.pendingWriters++
	f.mu.Unlock()
}

func (f *userFence) reserveReader() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writer || f.pendingWriters > 0 {
		return false
	}
	f.readers++
	return true
}

func (f *userFence) acquire(exclusive, reservedReader bool) {
	if reservedReader {
		return
	}
	f.mu.Lock()
	if exclusive {
		for f.writer || f.readers > 0 {
			f.cond.Wait()
		}
		f.pendingWriters--
		f.writer = true
	} else {
		for f.writer || f.pendingWriters > 0 {
			f.cond.Wait()
		}
		f.readers++
	}
	f.mu.Unlock()
}

func (f *userFence) release(exclusive bool) {
	f.mu.Lock()
	if exclusive {
		f.writer = false
	} else {
		f.readers--
	}
	f.cond.Broadcast()
	f.mu.Unlock()
}

// Broker schedules FIFO work per canonical-user/session lane while allowing
// unrelated lanes to use the worker pool concurrently.

type backgroundPermit struct {
	cancel context.CancelFunc
}

// Processor handles one typed agent request.

func (b *Broker) reserveFences(userIDs []string, exclusive bool) ([]*userFence, []bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fences := make([]*userFence, 0, len(userIDs))
	reservedReaders := make([]bool, 0, len(userIDs))
	for _, userID := range userIDs {
		fence := b.userFences[userID]
		if fence == nil {
			fence = newUserFence()
			b.userFences[userID] = fence
		}
		if exclusive {
			fence.reserveWriter()
			reservedReaders = append(reservedReaders, false)
		} else {
			reservedReaders = append(reservedReaders, fence.reserveReader())
		}
		fences = append(fences, fence)
	}
	return fences, reservedReaders
}

func (b *Broker) cancelFenceReservations(w *work) {
	if !w.exclusive {
		for i, reserved := range w.reservedReaders {
			if reserved && i < len(w.fences) {
				w.fences[i].release(false)
			}
		}
		return
	}
	for _, fence := range w.fences {
		fence.mu.Lock()
		fence.pendingWriters--
		fence.cond.Broadcast()
		fence.mu.Unlock()
	}
}

func normalizeUserIDs(userIDs []string) []string {
	seen := make(map[string]struct{}, len(userIDs))
	normalized := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			continue
		}
		if _, ok := seen[userID]; ok {
			continue
		}
		seen[userID] = struct{}{}
		normalized = append(normalized, userID)
	}
	sort.Strings(normalized)
	return normalized
}

func (b *Broker) RunUsersExclusive(ctx context.Context, canonicalUserIDs []string, operation func() error) error {
	userIDs := normalizeUserIDs(canonicalUserIDs)
	if len(userIDs) == 0 {
		return fmt.Errorf("broker exclusive operation requires a canonical user ID")
	}
	return b.runOperationForUsers(ctx, LaneKey{CanonicalUserID: userIDs[0], UserExclusive: true}, userIDs, operation)
}

func (b *Broker) runOperationForUsers(ctx context.Context, key LaneKey, userIDs []string, operation func() error) error {
	if operation == nil {
		return fmt.Errorf("broker lane operation is required")
	}
	done := make(chan error, 1)
	w := &work{key: key, run: operation, finish: func(err error) { done <- err }, exclusive: key.UserExclusive}
	w.fences, w.reservedReaders = b.reserveFences(userIDs, w.exclusive)
	if err := b.enqueue(w); err != nil {
		b.cancelFenceReservations(w)
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Broker) TryAcquireLowPriority(parent context.Context) (context.Context, func(), bool) {
	b.mu.Lock()
	if !b.accepting || !b.started || b.outstanding+b.steerCount != 0 || b.background != nil {
		b.mu.Unlock()
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	permit := &backgroundPermit{cancel: cancel}
	b.background = permit
	b.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			b.mu.Lock()
			if b.background == permit {
				b.background = nil
			}
			b.mu.Unlock()
		})
	}
	return ctx, release, true
}

// BeginDrain stops admission so new work (and steers) is rejected with the
// restart notice, and revokes the background permit so low-priority work
// frees capacity for the turns already running. Admitted lane work is
// untouched: running and queued turns keep their workers and fences.
// Idempotent; Shutdown remains the hard stop that cancels leftovers.
