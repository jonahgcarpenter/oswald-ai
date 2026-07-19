package usermemory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestSearchTranscriptEnforcesScopeDeliveryAndActiveSessionRetention(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "user-1", "user-2")
	active := bindTranscriptTestSession(t, store, "user-1", "session-1")
	otherSession := bindTranscriptTestSession(t, store, "user-1", "session-2")
	otherTenant := bindTranscriptTestSession(t, store, "user-2", "session-1")

	insertTranscriptTestTurn(t, store, "user-1", "session-1", active, "The launch code is quasar.", "I recorded quasar.", true, time.Hour)
	insertTranscriptTestTurn(t, store, "user-1", "session-1", active-1, "old generation quasar", "old assistant quasar", true, time.Hour)
	insertTranscriptTestTurn(t, store, "user-1", "session-2", otherSession, "other session quasar", "private", true, time.Hour)
	insertTranscriptTestTurn(t, store, "user-2", "session-1", otherTenant, "other tenant quasar", "private", true, time.Hour)
	insertTranscriptTestTurn(t, store, "user-1", "session-1", active, "undelivered quasar", "not visible", false, time.Hour)
	insertTranscriptTestTurn(t, store, "user-1", "session-1", active, "expired quasar", "not visible", true, -time.Hour)

	results, err := store.SearchTranscript(context.Background(), "user-1", "session-1", active, "quasar", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	var foundLaunch bool
	for _, got := range results {
		if got.SessionID != "session-1" || got.SessionGeneration != active || len(got.Records) != 2 || got.Records[0].Role != "user" || got.Records[1].Role != "assistant" {
			t.Fatalf("unexpected role-preserving excerpt: %+v", got)
		}
		if strings.Contains(got.Records[0].Content, "launch code") && strings.Contains(got.Records[1].Content, "recorded") {
			foundLaunch = true
		}
	}
	if !foundLaunch {
		t.Fatalf("launch exchange missing: %+v", results)
	}
}

func TestSearchTranscriptMatchesUserAndAssistantAndQuotesQuerySyntax(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "user-1")
	generation := bindTranscriptTestSession(t, store, "user-1", "session-1")
	insertTranscriptTestTurn(t, store, "user-1", "session-1", generation, "The user mentioned nebula.", "No special response.", true, time.Hour)
	insertTranscriptTestTurn(t, store, "user-1", "session-1", generation, "A different request.", "The answer mentioned pulsar.", true, time.Hour)

	for query, want := range map[string]string{
		`nebula" OR canonical_user_id:*`: "nebula",
		"pulsar":                         "pulsar",
	} {
		results, err := store.SearchTranscript(context.Background(), "user-1", "session-1", generation, query, 5)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if len(results) != 1 || !strings.Contains(results[0].Records[0].Content+results[0].Records[1].Content, want) {
			t.Fatalf("query %q results = %+v", query, results)
		}
	}
}

func TestSearchTranscriptRequiresCurrentActiveSessionAndGeneration(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "user-1")
	generation := bindTranscriptTestSession(t, store, "user-1", "session-1")
	insertTranscriptTestTurn(t, store, "user-1", "session-1", generation, "before reset marker", "before reset reply", true, time.Hour)
	if _, err := store.sql.Exec(`UPDATE tenant_sessions SET generation = generation + 1 WHERE canonical_user_id = 'user-1' AND session_id = 'session-1'`); err != nil {
		t.Fatal(err)
	}

	results, err := store.SearchTranscript(context.Background(), "user-1", "session-1", generation, "marker", 5)
	if err != nil || len(results) != 0 {
		t.Fatalf("stale generation results=%+v err=%v", results, err)
	}
	if _, err := store.sql.Exec(`UPDATE tenant_sessions SET expires_at = ? WHERE canonical_user_id = 'user-1' AND session_id = 'session-1'`, formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	results, err = store.SearchTranscript(context.Background(), "user-1", "session-1", generation+1, "marker", 5)
	if err != nil || len(results) != 0 {
		t.Fatalf("expired session results=%+v err=%v", results, err)
	}
}

func TestSearchTranscriptReturnsStableUnavailableError(t *testing.T) {
	store := newTranscriptTestStore(t)
	if _, err := store.sql.Exec(`DROP TABLE session_turns_fts`); err != nil {
		t.Fatal(err)
	}
	_, err := store.SearchTranscript(context.Background(), "user-1", "session-1", 1, "marker", 5)
	if !errors.Is(err, ErrTranscriptSearchUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestSearchTranscriptCapsQueryResultsAndOutput(t *testing.T) {
	store := newTranscriptTestStore(t)
	seedAccountUsers(t, store, "user-1")
	generation := bindTranscriptTestSession(t, store, "user-1", "session-1")
	for i := 0; i < maxTranscriptSearchLimit+5; i++ {
		insertTranscriptTestTurn(t, store, "user-1", "session-1", generation, "repeated marker", "short reply", true, time.Hour)
	}
	insertTranscriptTestTurn(t, store, "user-1", "session-1", generation, "marker "+strings.Repeat("x", maxTranscriptSearchChars), "oversized reply", true, time.Hour)

	results, err := store.SearchTranscript(context.Background(), "user-1", "session-1", generation, "marker", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != maxTranscriptSearchLimit {
		t.Fatalf("result count = %d", len(results))
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxTranscriptSearchChars {
		t.Fatalf("encoded results = %d chars", len(encoded))
	}

	query := strings.Repeat("term ", maxTranscriptQueryTerms+20)
	if got := strings.Count(transcriptMatchQuery(query), `"term"`); got != maxTranscriptQueryTerms {
		t.Fatalf("query term count = %d", got)
	}
}

func newTranscriptTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "oswald.db"), nil, "", config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func bindTranscriptTestSession(t *testing.T, store *Store, userID, sessionID string) int {
	t.Helper()
	profile, err := store.ResolveSessionProfile(context.Background(), userID, sessionID, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return profile.Generation
}

func insertTranscriptTestTurn(t *testing.T, store *Store, userID, sessionID string, generation int, userText, assistantText string, delivered bool, ttl time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	var deliveredAt any
	if delivered {
		deliveredAt = formatTime(now)
	}
	if _, err := store.sql.Exec(`
INSERT INTO session_turns (
	canonical_user_id, session_id, session_generation, user_text, assistant_text,
	created_at, expires_at, delivered_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, sessionID, generation, userText, assistantText,
		formatTime(now), formatTime(now.Add(ttl)), deliveredAt); err != nil {
		t.Fatal(err)
	}
}
