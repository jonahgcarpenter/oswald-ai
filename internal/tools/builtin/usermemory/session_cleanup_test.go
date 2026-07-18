package usermemory

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestCleanupExpiredSessionsIndependentSweep(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	ctx := context.Background()
	expiredProfile, err := store.ResolveSessionProfile(ctx, "user", "expired-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(ctx, "expired-session", "user", expiredProfile.Generation, "bound", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "independently-expired", "user", "expired", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurn(ctx, "subsecond-future", "user", "future", "turn", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveMemory(ctx, "user", SaveRequest{
		Scope: ScopeLongTerm, Category: "identity", Statement: "The user is Ada.", Confidence: 1, Importance: 5,
	}); err != nil {
		t.Fatal(err)
	}
	latestProfile, err := store.ResolveSessionProfile(ctx, "user", "active-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if latestProfile.VersionID == expiredProfile.VersionID {
		t.Fatal("profile mutation did not create a new version")
	}

	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE tenant_sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'expired-session'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'independently-expired'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE session_turns SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'subsecond-future'`, formatTime(now.Add(500*time.Millisecond))); err != nil {
		t.Fatal(err)
	}

	counts, err := store.CleanupExpiredSessions(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts != (SessionCleanupCounts{SessionTurnsDeleted: 2, TenantSessionsDeleted: 1, ProfileVersionsDeleted: 1}) {
		t.Fatalf("unexpected cleanup counts: %+v", counts)
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE session_id = 'subsecond-future'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM tenant_session_generations WHERE canonical_user_id = 'user' AND session_id = 'expired-session'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM tenant_profile_versions WHERE id = ?`, 1, latestProfile.VersionID)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM tenant_profile_versions WHERE id = ?`, 0, expiredProfile.VersionID)

	next, err := store.ResolveSessionProfile(ctx, "user", "expired-session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != expiredProfile.Generation+1 {
		t.Fatalf("next generation = %d, want %d", next.Generation, expiredProfile.Generation+1)
	}
}

func TestCleanupExpiredSessionsRollsBackOnFailure(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")

	profile, err := store.ResolveSessionProfile(context.Background(), "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSessionTurnForGeneration(context.Background(), "session", "user", profile.Generation, "user", "assistant", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.sql.Exec(`UPDATE tenant_sessions SET expires_at = ? WHERE canonical_user_id = 'user'`, formatTime(now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`CREATE TRIGGER fail_session_cleanup BEFORE DELETE ON tenant_sessions BEGIN SELECT RAISE(ABORT, 'cleanup blocked'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.CleanupExpiredSessions(context.Background(), now); err == nil {
		t.Fatal("cleanup unexpectedly succeeded")
	}
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user'`, 1)
	assertCleanupRowCount(t, store, `SELECT COUNT(*) FROM tenant_sessions WHERE canonical_user_id = 'user'`, 1)
}

func TestRunSessionCleanupImmediateRepeatsAndContinuesAfterError(t *testing.T) {
	cleaner := &recordingSessionCleaner{failFirst: true, delay: 5 * time.Millisecond, called: make(chan struct{}, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunSessionCleanup(ctx, cleaner, 2*time.Millisecond, nil)
		close(done)
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-cleaner.called:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for cleanup call %d", i+1)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup runner did not stop after cancellation")
	}

	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	if cleaner.calls < 3 {
		t.Fatalf("cleanup calls = %d, want at least 3", cleaner.calls)
	}
	if cleaner.maxActive != 1 {
		t.Fatalf("overlapping cleanups = %d", cleaner.maxActive)
	}
}

type recordingSessionCleaner struct {
	mu        sync.Mutex
	calls     int
	active    int
	maxActive int
	failFirst bool
	delay     time.Duration
	called    chan struct{}
}

func (c *recordingSessionCleaner) CleanupExpiredSessions(context.Context, time.Time) (SessionCleanupCounts, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.mu.Unlock()

	select {
	case c.called <- struct{}{}:
	default:
	}
	time.Sleep(c.delay)

	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	if c.failFirst && call == 1 {
		return SessionCleanupCounts{}, errors.New("transient cleanup failure")
	}
	return SessionCleanupCounts{SessionTurnsDeleted: 1}, nil
}

func assertCleanupRowCount(t *testing.T, store *Store, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := store.sql.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("row count = %d, want %d for %q", got, want, query)
	}
}
