package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestSessionContextSkipsLegacyFactsAndPreservesGeneration(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	defer store.Close() // nolint:errcheck
	seedAccountUsers(t, store, "user")
	if err := store.SyncSpeakerIntro("user", "Speaker"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.publishFixtureMemory(ctx, "user", memoryFixture{Scope: ScopeLongTerm, Category: "identity", Statement: "Legacy secret", Confidence: 1, Importance: 5}); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.ResolveSessionProfile(ctx, "user", "existing", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.FactCount != 1 {
		t.Fatalf("fixture did not bind legacy fact: %+v", legacy)
	}
	if _, err := store.sql.Exec(`UPDATE memory_entries SET confidence = 'not-a-number' WHERE canonical_user_id = 'user'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveSessionProfile(ctx, "user", "legacy-fails", time.Hour); err == nil {
		t.Fatal("legacy fact scanner unexpectedly accepted corrupt fact")
	}
	for _, id := range []string{"existing", "new"} {
		session, err := store.ResolveSessionContext(ctx, "user", id, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if session.SpeakerIntro != "Speaker" || session.Generation != 1 || session.IsNewSession != (id == "new") {
			t.Fatalf("%s: %+v", id, session)
		}
		var content, sources, renderer string
		var version, highWater int
		if err := store.sql.QueryRow(`SELECT rendered_content, source_memory_ids, renderer_version, profile_version, profile_version_high_water FROM sessions WHERE canonical_user_id = 'user' AND session_id = ?`, id).Scan(&content, &sources, &renderer, &version, &highWater); err != nil {
			t.Fatal(err)
		}
		if content != "" || sources != "[]" || renderer != "session-context-v1" || version < legacy.Version || highWater < version {
			t.Fatalf("%s bound legacy facts or lost version: content=%q sources=%q renderer=%q version=%d highWater=%d", id, content, sources, renderer, version, highWater)
		}
	}
	if err := store.SyncSpeakerIntro("user", "Updated"); err != nil {
		t.Fatal(err)
	}
	if err := store.appendFixtureDeliveredTurn(ctx, "existing", "user", 1, "old prompt", "old answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	again, err := store.ResolveSessionContext(ctx, "user", "existing", time.Hour)
	if err != nil || again.SpeakerIntro != "Speaker" || again.Generation != 1 {
		t.Fatalf("active context changed: %+v err=%v", again, err)
	}
	if _, err := store.sql.Exec(`UPDATE sessions SET expires_at = ? WHERE canonical_user_id = 'user' AND session_id = 'existing'`, formatTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ResolveSessionContext(ctx, "user", "existing", time.Hour)
	if err != nil || expired.Generation != 2 || expired.SpeakerIntro != "Updated" {
		t.Fatalf("expired context: %+v err=%v", expired, err)
	}
	if err := store.ResetSessionContext(ctx, "user", "existing"); err != nil {
		t.Fatal(err)
	}
	var turnCount int
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user' AND session_id = 'existing'`).Scan(&turnCount); err != nil || turnCount != 0 {
		t.Fatalf("reset retained turns: count=%d err=%v", turnCount, err)
	}
	reset, err := store.ResolveSessionContext(ctx, "user", "existing", time.Hour)
	if err != nil || reset.Generation != 3 {
		t.Fatalf("reset context: %+v err=%v", reset, err)
	}
	if err := store.appendFixtureDeliveredTurn(ctx, "existing", "user", 2, "stale", "stale answer", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.sql.QueryRow(`SELECT COUNT(*) FROM session_turns WHERE canonical_user_id = 'user' AND session_id = 'existing'`).Scan(&turnCount); err != nil || turnCount != 0 {
		t.Fatalf("stale turn survived reset: count=%d err=%v", turnCount, err)
	}
}
