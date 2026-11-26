package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStoreSaveLoad(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, ok, _ := store.Load(ctx); ok {
		t.Fatal("expected no token")
	}
	want := TokenSet{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct-1"}
	if err := store.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Load(ctx)
	if err != nil || !ok || got.AccessToken != want.AccessToken || got.AccountID != want.AccountID {
		t.Fatalf("load = %+v ok=%v err=%v", got, ok, err)
	}
	if Expired(got, time.Now()) {
		t.Fatal("fresh token reported expired")
	}
	expired := TokenSet{AccessToken: "x", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	if !Expired(expired, time.Now()) {
		t.Fatal("expired token not reported")
	}
}

func TestAccountIDFromIDToken(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"chatgpt_account_id": "acct-42"})
	token := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
	if got := AccountIDFromIDToken(token); got != "acct-42" {
		t.Fatalf("account id = %q", got)
	}
	if got := AccountIDFromIDToken("bad"); got != "" {
		t.Fatalf("bad token account id = %q", got)
	}
}

func TestClientFallsBackToStaticModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, TokenSet{AccessToken: "synthetic-access"}); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, root, "sub", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	models, err := client.ListModels(ctx)
	if err != nil || len(models) == 0 {
		t.Fatalf("static fallback = %v err=%v", models, err)
	}
}

func TestAuthorizeURLHasPKCE(t *testing.T) {
	url := BuildAuthorizeURL("http://localhost:1455/auth/callback", "state-1", "challenge-1")
	if !strings.Contains(url, "code_challenge=challenge-1") || !strings.Contains(url, "app_EMoamEEZ73f0CkXaXp7hrann") {
		t.Fatalf("authorize url = %q", url)
	}
}
