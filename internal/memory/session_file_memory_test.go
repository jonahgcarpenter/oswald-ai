package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestSessionFileMemoryFreezesPairUntilResetOrExpiry(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user", "other")
	session, err := store.ResolveSessionContext(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, bound, err := store.SessionFileMemory(ctx, "user", "session", session.Generation); err != nil || bound {
		t.Fatalf("unbound session: bound=%t err=%v", bound, err)
	}
	user, notes, err := store.BindSessionFileMemory(ctx, "user", "session", session.Generation, "profile", "notes")
	if err != nil || user != "profile" || notes != "notes" {
		t.Fatalf("first snapshot: %q %q %v", user, notes, err)
	}
	user, notes, err = store.BindSessionFileMemory(ctx, "user", "session", session.Generation, "new profile", "new notes")
	if err != nil || user != "profile" || notes != "notes" {
		t.Fatalf("snapshot was overwritten: %q %q %v", user, notes, err)
	}
	if _, _, _, err := store.SessionFileMemory(ctx, "other", "session", session.Generation); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-tenant snapshot: %v", err)
	}
	if _, _, err := store.BindSessionFileMemory(ctx, "user", "session", session.Generation+1, "wrong", "generation"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	if err := store.ResetSessionContext(ctx, "user", "session"); err != nil {
		t.Fatal(err)
	}
	reset, err := store.ResolveSessionContext(ctx, "user", "session", time.Hour)
	if err != nil || reset.Generation != session.Generation+1 {
		t.Fatalf("reset generation: %+v %v", reset, err)
	}
	if _, _, bound, err := store.SessionFileMemory(ctx, "user", "session", reset.Generation); err != nil || bound {
		t.Fatalf("reset retained snapshot: bound=%t err=%v", bound, err)
	}
	if _, _, err := store.BindSessionFileMemory(ctx, "user", "session", reset.Generation, "", ""); err != nil {
		t.Fatal(err)
	}
	if user, notes, bound, err := store.SessionFileMemory(ctx, "user", "session", reset.Generation); err != nil || !bound || user != "" || notes != "" {
		t.Fatalf("empty pair not frozen: %q %q %t %v", user, notes, bound, err)
	}
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ResolveSessionContext(ctx, "user", "session", time.Hour)
	if err != nil || expired.Generation != reset.Generation+1 {
		t.Fatalf("expired generation: %+v %v", expired, err)
	}
	if _, _, bound, err := store.SessionFileMemory(ctx, "user", "session", expired.Generation); err != nil || bound {
		t.Fatalf("expired session retained snapshot: bound=%t err=%v", bound, err)
	}
	if _, _, err := store.BindSessionFileMemory(ctx, "user", "session", expired.Generation, "new profile", "new notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'session'`, formatTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.cleanupExpiredSessions(ctx, time.Now().UTC(), config.DefaultRetentionPolicy()); err != nil {
		t.Fatal(err)
	}
	var oldUser, oldNotes sql.NullString
	if err := store.sql.QueryRow(`SELECT file_user_snapshot, file_memory_snapshot FROM sessions WHERE canonical_user_id = 'user' AND session_id = 'session'`).Scan(&oldUser, &oldNotes); err != nil || oldUser.Valid || oldNotes.Valid {
		t.Fatalf("inactive session retained private snapshot: user=%+v notes=%+v err=%v", oldUser, oldNotes, err)
	}
}

func TestSessionFileMemoryConcurrentFirstBindKeepsOnePair(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	session, err := store.ResolveSessionContext(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, pair := range [][2]string{{"first-user", "first-notes"}, {"second-user", "second-notes"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			user, notes, err := store.BindSessionFileMemory(ctx, "user", "session", session.Generation, pair[0], pair[1])
			if err == nil && (user == "first-user") != (notes == "first-notes") {
				err = errors.New("mixed memory snapshot pair")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionFileMemorySurvivesReopenWithoutRecapture(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	store := newTestStore(path, config.NewLogger(config.LevelError))
	seedAccountUsers(t, store, "user")
	session, err := store.ResolveSessionContext(ctx, "user", "session", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BindSessionFileMemory(ctx, "user", "session", session.Generation, "saved user", "saved notes"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newTestStore(path, config.NewLogger(config.LevelError))
	defer reopened.Close() // nolint:errcheck
	resolved, err := reopened.ResolveSessionContext(ctx, "user", "session", time.Hour)
	if err != nil || resolved.Generation != session.Generation {
		t.Fatalf("reopened session: %+v %v", resolved, err)
	}
	if user, notes, bound, err := reopened.SessionFileMemory(ctx, "user", "session", resolved.Generation); err != nil || !bound || user != "saved user" || notes != "saved notes" {
		t.Fatalf("reopened memory: %q %q %t %v", user, notes, bound, err)
	}
}
