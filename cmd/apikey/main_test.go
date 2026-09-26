package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/accounts"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
)

func TestCommandCreateAndRevoke(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oswald.db")
	db, err := database.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`INSERT INTO account_users(canonical_user_id) VALUES ('usr_test'); INSERT INTO linked_accounts(gateway, identifier, canonical_user_id) VALUES ('discord', '123', 'usr_test')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var out bytes.Buffer
	if err := run(ctx, path, []string{"create", "usr_test"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "key_id: ") || !strings.HasPrefix(lines[1], "token: osk_") {
		t.Fatalf("unexpected create output shape")
	}
	id := strings.TrimPrefix(lines[0], "key_id: ")
	token := strings.TrimPrefix(lines[1], "token: ")
	svc := accounts.NewService(path, nil, nil, nil)
	defer svc.Close()
	if p, err := svc.AuthenticateAPIKey(ctx, token); err != nil || p.CanonicalUserID != "usr_test" {
		t.Fatalf("issued token invalid: %+v %v", p, err)
	}
	out.Reset()
	if err := run(ctx, path, []string{"revoke", id}, &out); err != nil || out.String() != "revoked\n" {
		t.Fatalf("revoke output=%q err=%v", out.String(), err)
	}
	if _, err := svc.AuthenticateAPIKey(ctx, token); !errors.Is(err, accounts.ErrInvalidAPIKey) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	out.Reset()
	if err := run(ctx, path, []string{"create", "missing"}, &out); err == nil || out.Len() != 0 {
		t.Fatal("missing user issued token")
	}
	for _, invalid := range []string{"not-a-key-id", strings.ToUpper(id), "osk_" + id + "_" + strings.Repeat("a", 64)} {
		out.Reset()
		if err := run(ctx, path, []string{"revoke", invalid}, &out); !errors.Is(err, accounts.ErrInvalidAPIKey) || out.Len() != 0 {
			t.Fatalf("invalid revoke ID: err=%v output length=%d", err, out.Len())
		}
	}
}
