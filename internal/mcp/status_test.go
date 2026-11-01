package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTokenStateLifecycle(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	present, expired, err := TokenState(ctx, root, "docs")
	if err != nil || present || expired {
		t.Fatalf("empty state = %v %v %v", present, expired, err)
	}
	store, err := newTokenStore(root, "docs")
	if err != nil {
		t.Fatal(err)
	}
	future := TokenSet{AccessToken: "a", TokenType: "Bearer", Issuer: "https://example.com", Expiry: time.Now().Add(time.Hour)}
	if err := store.SaveTokens(ctx, future); err != nil {
		t.Fatal(err)
	}
	present, expired, err = TokenState(ctx, root, "docs")
	if err != nil || !present || expired {
		t.Fatalf("fresh state = %v %v %v", present, expired, err)
	}
	past := TokenSet{AccessToken: "a", TokenType: "Bearer", Issuer: "https://example.com", Expiry: time.Now().Add(-time.Hour)}
	if err := store.SaveTokens(ctx, past); err != nil {
		t.Fatal(err)
	}
	present, expired, err = TokenState(ctx, root, "docs")
	if err != nil || !present || !expired {
		t.Fatalf("stale state = %v %v %v", present, expired, err)
	}
	if _, _, err := TokenState(ctx, root, "Bad Name!"); err == nil {
		t.Fatal("bad server name accepted")
	}
}

func TestRemoveServerTokens(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	store, err := newTokenStore(root, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTokens(ctx, TokenSet{AccessToken: "a", TokenType: "Bearer", Issuer: "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	dropped, err := RemoveServerTokens(root, "docs")
	if err != nil || !dropped {
		t.Fatalf("remove = %v %v", dropped, err)
	}
	if _, err := os.Stat(filepath.Join(root, "mcp", "docs")); !os.IsNotExist(err) {
		t.Fatal("token dir survived")
	}
	dropped, err = RemoveServerTokens(root, "docs")
	if err != nil || dropped {
		t.Fatalf("second remove = %v %v", dropped, err)
	}
	if _, err := RemoveServerTokens(root, "Bad Name!"); err == nil {
		t.Fatal("bad server name accepted")
	}
}
