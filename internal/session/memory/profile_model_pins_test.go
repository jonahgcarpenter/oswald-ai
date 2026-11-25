package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSessionModelPinRoundTrip(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:555000111"
	if err := s.SetSessionModel(ctx, "alice", key, "openrouter", "example/model-v1"); err != nil {
		t.Fatal(err)
	}
	provider, model, found, err := s.SessionModel(ctx, "alice", key)
	if err != nil || !found || provider != "openrouter" || model != "example/model-v1" {
		t.Fatalf("read = %q %q found=%v err=%v", provider, model, found, err)
	}
	// The row is conversation-keyed, never addressed through sessions.id.
	var stored string
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key=?`, "oswald:v1:session_model:discord:dm:555000111").Scan(&stored); err != nil {
		t.Fatalf("persisted row missing: %v", err)
	}
	if stored != `{"version":1,"provider":"openrouter","model":"example/model-v1"}` {
		t.Fatalf("encoded row = %s", stored)
	}
	// Setting another model replaces the pin; the custom: selector prefix is
	// normalized away so only the bare provider name is stored.
	if err := s.SetSessionModel(ctx, "alice", key, "custom:example-gateway", "example/model-v2"); err != nil {
		t.Fatal(err)
	}
	provider, model, found, err = s.SessionModel(ctx, "alice", key)
	if err != nil || !found || provider != "example-gateway" || model != "example/model-v2" {
		t.Fatalf("replaced read = %q %q found=%v err=%v", provider, model, found, err)
	}
}

func TestSessionModelPinAbsent(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	provider, model, found, err := s.SessionModel(ctx, "alice", "discord:dm:555000111")
	if err != nil || found || provider != "" || model != "" {
		t.Fatalf("absent pin = %q %q found=%v err=%v", provider, model, found, err)
	}
}

func TestSessionModelPinCorruptFailsClosed(t *testing.T) {
	key := "discord:dm:555000111"
	rowKey := "oswald:v1:session_model:discord:dm:555000111"
	cases := []struct{ name, value, field string }{
		{"malformed", `not-json-canary`, "json"},
		{"empty", ``, "json"},
		{"wrong version", `{"version":2,"provider":"openrouter","model":"canary-model"}`, "version"},
		{"missing provider", `{"version":1,"model":"canary-model"}`, "provider"},
		{"missing model", `{"version":1,"provider":"openrouter"}`, "model"},
		{"whitespace provider", `{"version":1,"provider":"   ","model":"canary-model"}`, "provider"},
		{"unknown field", `{"version":1,"provider":"openrouter","model":"canary-model","extra":true}`, "json"},
		{"trailing value", `{"version":1,"provider":"openrouter","model":"canary-model"}{}`, "json"},
		{"oversize", `{"version":1,"provider":"openrouter","model":"` + strings.Repeat("m", 1024) + `"}`, "size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newProfileStateFixture(t)
			ctx := context.Background()
			if _, err := s.db.SQL().ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,?)`, rowKey, tc.value); err != nil {
				t.Fatal(err)
			}
			provider, model, found, err := s.SessionModel(ctx, "alice", key)
			if err == nil {
				t.Fatalf("corrupt pin accepted as %q %q", provider, model)
			}
			if found || provider != "" || model != "" {
				t.Fatalf("corrupt pin reported present: %q %q found=%v", provider, model, found)
			}
			if !errors.Is(err, errSessionModelValue) {
				t.Fatalf("corrupt pin error lost the sentinel classification: %v", err)
			}
			if want := errSessionModelValue.Error() + ": " + tc.field; err.Error() != want {
				t.Fatalf("error text = %q, want %q", err.Error(), want)
			}
			var marker interface{ UnreadablePin() bool }
			if !errors.As(err, &marker) || !marker.UnreadablePin() {
				t.Fatalf("corrupt pin error is not marked unreadable: %v", err)
			}
			for _, canary := range []string{"canary", "openrouter"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("error leaked stored value: %v", err)
				}
			}
		})
	}
	t.Run("null", func(t *testing.T) {
		s, _ := newProfileStateFixture(t)
		ctx := context.Background()
		if _, err := s.db.SQL().ExecContext(ctx, `INSERT INTO state_meta(key,value) VALUES(?,NULL)`, rowKey); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.SessionModel(ctx, "alice", key); err == nil || !errors.Is(err, errSessionModelValue) {
			t.Fatalf("null pin err = %v", err)
		}
	})
}

func TestSessionModelPinStoreFailuresAreNotUnreadable(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	var marker interface{ UnreadablePin() bool }
	writeErr := s.SetSessionModel(ctx, "alice", "slack:dm:555000111", "openrouter", "example/model")
	if writeErr == nil || errors.Is(writeErr, errSessionModelValue) || errors.As(writeErr, &marker) {
		t.Fatalf("scope write failure classified as unreadable: %v", writeErr)
	}
	_, _, _, readErr := s.SessionModel(ctx, "alice", "slack:dm:555000111")
	if readErr == nil || errors.Is(readErr, errSessionModelValue) || errors.As(readErr, &marker) {
		t.Fatalf("scope read failure classified as unreadable: %v", readErr)
	}
	// Invalid write arguments are caller errors, not unreadable stored values.
	argumentErr := s.SetSessionModel(ctx, "alice", "discord:dm:555000111", "", "example/model")
	if argumentErr == nil || errors.Is(argumentErr, errSessionModelValue) || errors.As(argumentErr, &marker) {
		t.Fatalf("invalid write arguments classified as unreadable: %v", argumentErr)
	}
}

func TestSessionModelPinRejectsInvalidWrites(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:555000111"
	cases := []struct{ name, provider, model string }{
		{"empty provider", "", "example/model"},
		{"whitespace provider", " \t", "example/model"},
		{"empty model", "openrouter", ""},
		{"whitespace model", "openrouter", "   "},
		{"oversize provider", strings.Repeat("p", 257), "example/model"},
		{"oversize model", "openrouter", strings.Repeat("m", 257)},
		{"oversize encoding", strings.Repeat("é", 256), strings.Repeat("é", 256)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SetSessionModel(ctx, "alice", key, tc.provider, tc.model); err == nil {
				t.Fatal("invalid pin accepted")
			}
		})
	}
	var count int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM state_meta WHERE substr(key,1,?)=?`, len(sessionModelPrefix), sessionModelPrefix).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected writes persisted: count=%d err=%v", count, err)
	}
	if _, _, found, err := s.SessionModel(ctx, "alice", key); err != nil || found {
		t.Fatalf("rejected writes created a pin: found=%v err=%v", found, err)
	}
}

func TestSessionModelPinScopeFencing(t *testing.T) {
	s, _ := newProfileStateFixture(t)
	ctx := context.Background()
	cases := []struct{ name, owner, key string }{
		{"wrong owner", "bob", "discord:dm:555000111"},
		{"unknown source", "alice", "slack:dm:555000111"},
		{"empty key", "alice", ""},
		{"untrimmed key", "alice", " discord:dm:555000111"},
		{"oversized key", "alice", "discord:" + strings.Repeat("a", 1024)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SetSessionModel(ctx, tc.owner, tc.key, "openrouter", "example/model"); err == nil {
				t.Fatal("out-of-scope write accepted")
			}
			if _, _, _, err := s.SessionModel(ctx, tc.owner, tc.key); err == nil {
				t.Fatal("out-of-scope read accepted")
			}
		})
	}
	var count int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM state_meta WHERE substr(key,1,?)=?`, len(sessionModelPrefix), sessionModelPrefix).Scan(&count); err != nil || count != 0 {
		t.Fatalf("out-of-scope writes persisted: count=%d err=%v", count, err)
	}
}

func TestSessionModelPinSurvivesNewSessionAndSweep(t *testing.T) {
	s, root := newProfileStateFixture(t)
	ctx := context.Background()
	key := "discord:dm:555000111"
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.SetSessionModel(ctx, "alice", key, "openrouter", "example/model-v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSessionContext(ctx, "alice", key); err != nil {
		t.Fatal(err)
	}
	session, err := s.ResolveSessionContext(ctx, "alice", key, time.Hour)
	if err != nil || session.Generation != 2 {
		t.Fatalf("new session = %+v err=%v", session, err)
	}
	provider, model, found, err := s.SessionModel(ctx, "alice", key)
	if err != nil || !found || provider != "openrouter" || model != "example/model-v1" {
		t.Fatalf("pin lost after /new: %q %q found=%v err=%v", provider, model, found, err)
	}
	// Ordinary TTL expiry sweeps the transcript and session rows, never the pin.
	now = now.Add(3 * time.Hour)
	if err := s.SweepProfile(ctx); err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE expiry_finalized=1`).Scan(&sessions); err != nil || sessions == 0 {
		t.Fatalf("session not expired: count=%d err=%v", sessions, err)
	}
	if provider, model, found, err := s.SessionModel(ctx, "alice", key); err != nil || !found || provider != "openrouter" || model != "example/model-v1" {
		t.Fatalf("pin lost after sweep: %q %q found=%v err=%v", provider, model, found, err)
	}
	// The pin is state_meta, so it also survives a store restart.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewProfileStore(ctx, root, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if provider, model, found, err := reopened.SessionModel(ctx, "alice", key); err != nil || !found || provider != "openrouter" || model != "example/model-v1" {
		t.Fatalf("pin lost after restart: %q %q found=%v err=%v", provider, model, found, err)
	}
}
