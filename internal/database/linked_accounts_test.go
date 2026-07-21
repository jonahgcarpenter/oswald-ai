package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

func TestReplaceAccountLinksPreservesUserMemoryRows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "oswald.db"), config.NewLogger(config.LevelError))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close() // nolint:errcheck

	now := time.Now().UTC()
	data := AccountLinkData{
		Users: map[string]AccountUser{
			"usr_test": {
				CreatedAt: now,
				UpdatedAt: now,
				Accounts:  []LinkedAccount{{Gateway: "websocket", Identifier: "user", LinkedAt: now}},
			},
		},
	}
	if err := db.ReplaceAccountLinks(data); err != nil {
		t.Fatalf("initial replace: %v", err)
	}
	if _, err := db.SQL().Exec(`UPDATE account_users SET speaker_intro = ? WHERE canonical_user_id = ?`, "You are speaking with Test User.", "usr_test"); err != nil {
		t.Fatalf("insert profile: %v", err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO memory_entries (canonical_user_id, scope, category, statement, statement_key, evidence, confidence, importance, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "usr_test", "long_term", "durable_preferences", "The user likes purple.", "the user likes purple.", "test evidence", 0.9, 3, "active", formatDBTime(now), formatDBTime(now)); err != nil {
		t.Fatalf("insert entry: %v", err)
	}

	data.Users["usr_test"] = AccountUser{
		CreatedAt: now,
		UpdatedAt: now.Add(time.Minute),
		Accounts:  []LinkedAccount{{Gateway: "websocket", Identifier: "user", DisplayName: "User", LinkedAt: now}},
	}
	if err := db.ReplaceAccountLinks(data); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	var profileCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM account_users WHERE canonical_user_id = ? AND speaker_intro != ''`, "usr_test").Scan(&profileCount); err != nil {
		t.Fatalf("count profiles: %v", err)
	}
	var entryCount int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE canonical_user_id = ?`, "usr_test").Scan(&entryCount); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	if profileCount != 1 || entryCount != 1 {
		t.Fatalf("expected memory preserved, got profiles=%d entries=%d", profileCount, entryCount)
	}
}
