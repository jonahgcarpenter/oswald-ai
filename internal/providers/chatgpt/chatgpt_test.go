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

func TestListModelsUnauthorized(t *testing.T) {
	root := t.TempDir()
	client, err := NewClient("https://chatgpt.example.com", root, "sub", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListModels(context.Background()); err == nil {
		t.Fatal("expected error for unauthorized subscription")
	}
}

func TestAuthorizeURLHasPKCE(t *testing.T) {
	url := BuildAuthorizeURL("http://localhost:1455/auth/callback", "state-1", "challenge-1")
	if !strings.Contains(url, "code_challenge=challenge-1") || !strings.Contains(url, "app_EMoamEEZ73f0CkXaXp7hrann") {
		t.Fatalf("authorize url = %q", url)
	}
}

func TestPendingRoundTrip(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, ok, _ := store.LoadPending(ctx); ok {
		t.Fatal("expected no pending handoff")
	}
	want := PendingAuth{Verifier: "synthetic-verifier", State: "synthetic-state", RedirectURI: "http://localhost:1455/auth/callback", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := store.SavePending(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.LoadPending(ctx)
	if err != nil || !ok || got.Verifier != want.Verifier || got.State != want.State {
		t.Fatalf("pending = %+v ok=%v err=%v", got, ok, err)
	}
	if PendingExpired(got, time.Now()) {
		t.Fatal("fresh pending reported expired")
	}
	if !PendingExpired(PendingAuth{ExpiresAt: time.Now().Add(-time.Hour).Unix()}, time.Now()) {
		t.Fatal("expired pending not reported")
	}
	if err := store.ClearPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.LoadPending(ctx); ok {
		t.Fatal("pending not cleared")
	}
}

func TestExtractCode(t *testing.T) {
	if got, err := ExtractCode("synthetic-code", "s"); err != nil || got != "synthetic-code" {
		t.Fatalf("bare code = %q err=%v", got, err)
	}
	if got, err := ExtractCode("http://localhost:1455/auth/callback?code=synthetic-code&state=s", "s"); err != nil || got != "synthetic-code" {
		t.Fatalf("redirect url = %q err=%v", got, err)
	}
	if _, err := ExtractCode("http://localhost:1455/auth/callback?code=x&state=other", "s"); err == nil {
		t.Fatal("expected state mismatch")
	}
	if _, err := ExtractCode("http://localhost:1455/auth/callback?error=access_denied&state=s", "s"); err == nil {
		t.Fatal("expected denial error")
	}
	if _, err := ExtractCode("", "s"); err == nil {
		t.Fatal("expected empty error")
	}
}

func TestStartFinishHandoff(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"chatgpt_account_id": "acct-7"})
	idToken := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"id_token":"` + idToken + `"}`))
	}))
	defer server.Close()
	old := authBaseURL
	authBaseURL = server.URL
	defer func() { authBaseURL = old }()

	root := t.TempDir()
	ctx := context.Background()
	authURL, _, err := Start(ctx, root, "sub", time.Hour)
	if err != nil || !strings.Contains(authURL, "oauth/authorize") {
		t.Fatalf("start = %q err=%v", authURL, err)
	}
	if err := Finish(ctx, root, "sub", "synthetic-code", nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	store, err := NewStore(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	tokens, ok, err := store.Load(ctx)
	if err != nil || !ok || tokens.AccessToken != "synthetic-access" || tokens.AccountID != "acct-7" {
		t.Fatalf("tokens = %+v ok=%v err=%v", tokens, ok, err)
	}
	if _, ok, _ := store.LoadPending(ctx); ok {
		t.Fatal("pending not cleared after finish")
	}
}

func TestFinishWithoutStart(t *testing.T) {
	if err := Finish(context.Background(), t.TempDir(), "sub", "synthetic-code", nil); err == nil {
		t.Fatal("expected error without pending handoff")
	}
}
