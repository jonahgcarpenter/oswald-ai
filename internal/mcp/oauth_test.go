package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeAuthorizationServer emulates an OAuth authorization server plus its
// protected-resource metadata: metadata discovery, auto-approving authorize
// endpoint, PKCE-checked token exchange, dynamic registration, and device flow.
type fakeAuthorizationServer struct {
	t            *testing.T
	server       *httptest.Server
	mu           sync.Mutex
	cimdAllowed  bool
	registered   map[string]bool
	tokenPolls   int64
	deviceOKAt   int64
	refreshCount int64
}

func newFakeAuthorizationServer(t *testing.T, cimdAllowed bool) *fakeAuthorizationServer {
	t.Helper()
	fake := &fakeAuthorizationServer{t: t, cimdAllowed: cimdAllowed, registered: map[string]bool{}}
	mux := http.NewServeMux()
	issuer := ""
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}})
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/authorize",
			"token_endpoint":                        issuer + "/token",
			"registration_endpoint":                 issuer + "/register",
			"device_authorization_endpoint":         issuer + "/device",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"client_id_metadata_document_supported": fake.cimdAllowed,
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var meta struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil || len(meta.RedirectURIs) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		fake.registered["dcr-client"] = true
		fake.mu.Unlock()
		writeJSON(w, map[string]any{"client_id": "dcr-client", "redirect_uris": meta.RedirectURIs, "token_endpoint_auth_method": "none"})
	})
	// Auto-approving authorize endpoint: redirects straight back with a code.
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		redirect, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		params := redirect.Query()
		params.Set("code", "auth-code-"+query.Get("client_id"))
		params.Set("state", query.Get("state"))
		redirect.RawQuery = params.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		grant := r.PostForm.Get("grant_type")
		clientID := r.PostForm.Get("client_id")
		if clientID == "" {
			if user, _, ok := r.BasicAuth(); ok {
				clientID = user
			}
		}
		allowed := clientID == "operator-client" || clientID == "dcr-client" || (fake.cimdAllowed && strings.HasPrefix(clientID, "http://127.0.0.1:"))
		switch grant {
		case "authorization_code":
			if r.PostForm.Get("code_verifier") == "" {
				writeOAuthError(w, "invalid_request")
				return
			}
			if !allowed {
				writeOAuthError(w, "invalid_client")
				return
			}
			if r.PostForm.Get("code") != "auth-code-"+clientID {
				writeOAuthError(w, "invalid_grant")
				return
			}
			writeJSON(w, map[string]any{"access_token": "access-" + clientID, "token_type": "Bearer", "refresh_token": "refresh-" + clientID, "expires_in": 3600})
		case "refresh_token":
			atomic.AddInt64(&fake.refreshCount, 1)
			if !allowed || r.PostForm.Get("refresh_token") != "refresh-"+clientID {
				writeOAuthError(w, "invalid_grant")
				return
			}
			writeJSON(w, map[string]any{"access_token": "rotated-" + clientID, "token_type": "Bearer", "refresh_token": "refresh-" + clientID, "expires_in": 3600})
		case "urn:ietf:params:oauth:grant-type:device_code":
			polls := atomic.AddInt64(&fake.tokenPolls, 1)
			if polls < fake.deviceOKAt {
				writeOAuthError(w, "authorization_pending")
				return
			}
			if r.PostForm.Get("device_code") != "device-code-1" {
				writeOAuthError(w, "invalid_grant")
				return
			}
			writeJSON(w, map[string]any{"access_token": "access-device", "token_type": "Bearer", "refresh_token": "refresh-device", "expires_in": 3600})
		default:
			writeOAuthError(w, "unsupported_grant_type")
		}
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"device_code":      "device-code-1",
			"user_code":        "USER-CODE",
			"verification_uri": issuer + "/activate",
			"expires_in":       300,
			"interval":         0,
		})
	})
	server := httptest.NewServer(mux)
	issuer = server.URL
	fake.server = server
	return fake
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeOAuthError(w http.ResponseWriter, code string) {
	w.WriteHeader(http.StatusBadRequest)
	writeJSON(w, map[string]any{"error": code})
}

func (f *fakeAuthorizationServer) close() { f.server.Close() }

func (f *fakeAuthorizationServer) mcpURL() string { return f.server.URL + "/mcp" }

// syncBuffer is a goroutine-safe strings.Builder for tests where a flow
// goroutine writes prompts while the test goroutine polls the output.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testOAuthStore(t *testing.T) (*tokenStore, string) {
	t.Helper()
	root := t.TempDir()
	store, err := newTokenStore(root, "oauthsrv")
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func TestOAuthDiscoveryFindsEndpoints(t *testing.T) {
	fake := newFakeAuthorizationServer(t, true)
	defer fake.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Issuer != fake.server.URL || meta.TokenEndpoint != fake.server.URL+"/token" || meta.AuthorizationEndpoint != fake.server.URL+"/authorize" {
		t.Fatalf("metadata = %+v", meta)
	}
	if meta.DeviceAuthorizationEndpoint != fake.server.URL+"/device" || !meta.CIMDSupported {
		t.Fatalf("metadata = %+v", meta)
	}
}

func TestBrowserFlowCIMDLoopbackRoundTrip(t *testing.T) {
	fake := newFakeAuthorizationServer(t, true)
	defer fake.close()
	store, _ := testOAuthStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	// Complete the browser redirect without a browser: follow the printed URL.
	var printed syncBuffer
	done := make(chan struct{})
	var flowErr error
	var tokens *TokenSet
	go func() {
		defer close(done)
		oauth := &OAuthConfig{Flow: "browser", Timeout: time.Minute}
		tokens, flowErr = RunBrowserFlow(ctx, store, oauth, meta, fake.mcpURL(), FlowOptions{NoBrowser: true, Stdout: &printed})
	}()
	deadline := time.Now().Add(20 * time.Second)
	var authURL string
	for time.Now().Before(deadline) {
		text := printed.String()
		if i := strings.Index(text, "http://"); i >= 0 {
			authURL = strings.TrimSpace(text[i:])
			if j := strings.Index(authURL, "\n"); j >= 0 {
				authURL = authURL[:j]
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if authURL == "" {
		t.Fatalf("no authorization URL printed: %q", printed.String())
	}
	response, err := http.Get(authURL)
	for i := 0; i < 200 && err != nil; i++ {
		select {
		case <-done:
			break
		default:
		}
		time.Sleep(10 * time.Millisecond)
		response, err = http.Get(authURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authorize status = %d", response.StatusCode)
	}
	<-done
	if flowErr != nil {
		t.Fatal(flowErr)
	}
	if tokens.AccessToken != "access-"+tokens.ClientID || !strings.HasPrefix(tokens.ClientID, "http://127.0.0.1:") {
		t.Fatalf("tokens = %+v", tokens)
	}
	if tokens.Issuer != fake.server.URL {
		t.Fatalf("issuer = %q", tokens.Issuer)
	}
	// Persistence is the caller's job (AuthorizeServer in production).
	if err := store.SaveTokens(ctx, *tokens); err != nil {
		t.Fatal(err)
	}
	saved, ok, err := store.LoadTokens(ctx)
	if err != nil || !ok || saved.AccessToken != tokens.AccessToken {
		t.Fatalf("stored tokens = %+v %t %v", saved, ok, err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "oauthsrv.json")); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserFlowCIMDRefusalFallsBackToDCR(t *testing.T) {
	fake := newFakeAuthorizationServer(t, false)
	defer fake.close()
	store, _ := testOAuthStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	if meta.CIMDSupported {
		t.Fatal("fake should not advertise CIMD support")
	}
	// Metadata says unsupported, so resolution must require DCR immediately.
	if _, err := resolveOAuthClient(ctx, &OAuthConfig{}, store, meta, "http://127.0.0.1:9/callback"); err != errNoCIMD {
		t.Fatalf("resolve = %v", err)
	}
	client, err := performDCR(ctx, store, meta, "http://127.0.0.1:9/callback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientID != "dcr-client" || client.Method != clientMethodDCR {
		t.Fatalf("client = %+v", client)
	}
	if record, ok, err := store.LoadClient(ctx); err != nil || !ok || record.ClientID != "dcr-client" {
		t.Fatalf("client record = %+v %t %v", record, ok, err)
	}
	// A recorded refusal is honored on later resolutions.
	if err := store.DisableCIMD(ctx); err != nil {
		t.Fatal(err)
	}
	if disabled, err := store.CIMDDisabled(ctx); err != nil || !disabled {
		t.Fatalf("cimd marker = %t %v", disabled, err)
	}
}

func TestBrowserFlowInvalidClientRetriesWithDCR(t *testing.T) {
	// Server advertises CIMD but rejects the loopback client at the token
	// endpoint; the flow must mark CIMD off and complete via DCR.
	fake := newFakeAuthorizationServer(t, true)
	defer fake.close()
	store, _ := testOAuthStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	fake.cimdAllowed = false
	var printed syncBuffer
	done := make(chan struct{})
	var flowErr error
	var tokens *TokenSet
	go func() {
		defer close(done)
		oauth := &OAuthConfig{Flow: "browser", Timeout: time.Minute}
		tokens, flowErr = RunBrowserFlow(ctx, store, oauth, meta, fake.mcpURL(), FlowOptions{NoBrowser: true, Stdout: &printed})
	}()
	// The flow binds two listeners (CIMD attempt, then DCR); follow every
	// printed authorization URL until the flow completes.
	seen := map[string]bool{}
	deadline := time.Now().Add(30 * time.Second)
flow:
	for time.Now().Before(deadline) {
		select {
		case <-done:
			break flow
		default:
		}
		for _, line := range strings.Split(printed.String(), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "http://") || seen[line] {
				continue
			}
			response, err := http.Get(line)
			if err != nil {
				continue
			}
			response.Body.Close()
			seen[line] = true
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done
	if flowErr != nil {
		t.Fatal(flowErr)
	}
	if tokens.ClientID != "dcr-client" || !strings.HasPrefix(tokens.AccessToken, "access-") {
		t.Fatalf("tokens = %+v", tokens)
	}
	if disabled, err := store.CIMDDisabled(ctx); err != nil || !disabled {
		t.Fatalf("cimd marker = %t %v", disabled, err)
	}
}

func TestDeviceFlowPollsToCompletion(t *testing.T) {
	fake := newFakeAuthorizationServer(t, true)
	fake.deviceOKAt = 2
	defer fake.close()
	store, _ := testOAuthStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	var printed syncBuffer
	tokens, err := RunDeviceFlow(ctx, store, &OAuthConfig{Flow: "device", Timeout: time.Minute}, meta, fake.mcpURL(), FlowOptions{Stdout: &printed})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access-device" || tokens.RefreshToken != "refresh-device" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if !strings.Contains(printed.String(), "USER-CODE") {
		t.Fatalf("user code not printed: %q", printed.String())
	}
	if atomic.LoadInt64(&fake.tokenPolls) < 2 {
		t.Fatalf("token polls = %d", fake.tokenPolls)
	}
}

func TestDeviceFlowRequiresAdvertisedEndpoint(t *testing.T) {
	store, _ := testOAuthStore(t)
	ctx := context.Background()
	meta := &serverMetadata{Issuer: "https://example.com", AuthorizationEndpoint: "https://example.com/authorize", TokenEndpoint: "https://example.com/token"}
	if _, err := RunDeviceFlow(ctx, store, &OAuthConfig{}, meta, "https://example.com/mcp", FlowOptions{Stdout: io.Discard}); err == nil {
		t.Fatal("device flow without endpoint accepted")
	}
}

func TestOAuthRefreshPersistsRotatedTokens(t *testing.T) {
	fake := newFakeAuthorizationServer(t, true)
	defer fake.close()
	store, _ := testOAuthStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	stored := TokenSet{AccessToken: "stale", RefreshToken: "refresh-operator-client", Expiry: time.Now().Add(-time.Hour), Issuer: fake.server.URL, ClientID: "operator-client"}
	if err := store.SaveTokens(ctx, stored); err != nil {
		t.Fatal(err)
	}
	client := &resolvedOAuthClient{ClientID: "operator-client", Method: clientMethodOperator, AuthStyle: oauth2.AuthStyleInParams}
	refreshed, err := refreshOAuthToken(ctx, store, meta, client, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "rotated-operator-client" {
		t.Fatalf("refreshed = %+v", refreshed)
	}
	saved, _, err := store.LoadTokens(ctx)
	if err != nil || saved.AccessToken != "rotated-operator-client" {
		t.Fatalf("stored = %+v %v", saved, err)
	}
	if atomic.LoadInt64(&fake.refreshCount) != 1 {
		t.Fatalf("refresh calls = %d", fake.refreshCount)
	}
}

func TestOAuthRuntimeHandlerServesAndRefreshes(t *testing.T) {
	fake := newFakeAuthorizationServer(t, true)
	defer fake.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	newRuntime := func(tokens *TokenSet) *oauthRuntime {
		t.Helper()
		// Each case gets an isolated profile root so stored credentials never
		// leak between cases sharing one server name.
		isolated := t.TempDir()
		cfg := ServerConfig{Name: "oauthsrv", Transport: TransportStreamableHTTP, URL: fake.mcpURL(), OAuth: &OAuthConfig{Flow: "browser"}, Enabled: true}
		runtime, err := newOAuthRuntime(isolated, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.store.SaveMetadata(ctx, *meta); err != nil {
			t.Fatal(err)
		}
		if tokens != nil {
			if err := runtime.store.SaveTokens(ctx, *tokens); err != nil {
				t.Fatal(err)
			}
		}
		return runtime
	}
	// Fresh tokens are served without network use.
	fresh := newRuntime(&TokenSet{AccessToken: "live-token", Issuer: fake.server.URL, ClientID: "operator-client", Expiry: time.Now().Add(time.Hour)})
	source, err := fresh.TokenSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token()
	if err != nil || token.AccessToken != "live-token" {
		t.Fatalf("token = %+v %v", token, err)
	}
	// Expired tokens refresh through the stored credential.
	stale := newRuntime(&TokenSet{AccessToken: "stale", RefreshToken: "refresh-operator-client", Expiry: time.Now().Add(-time.Hour), Issuer: fake.server.URL, ClientID: "operator-client"})
	source, err = stale.TokenSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token, err = source.Token(); err != nil || token.AccessToken != "rotated-operator-client" {
		t.Fatalf("token = %+v %v", token, err)
	}
	// Missing tokens fail closed with a re-login prompt.
	empty := newRuntime(nil)
	if _, err := empty.TokenSource(ctx); err == nil || !strings.Contains(err.Error(), "oswald mcp auth") {
		t.Fatalf("expected re-login error, got %v", err)
	}
	// A dead refresh token surfaces the same prompt through Authorize.
	dead := newRuntime(&TokenSet{AccessToken: "stale", RefreshToken: "refresh-unknown", Expiry: time.Now().Add(-time.Hour), Issuer: fake.server.URL, ClientID: "operator-client"})
	response := &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody, Header: http.Header{}}
	if err := dead.Authorize(ctx, &http.Request{}, response); err == nil || !strings.Contains(err.Error(), "oswald mcp auth") {
		t.Fatalf("expected re-login error, got %v", err)
	}
	// Issuer drift fails closed instead of sending tokens elsewhere.
	moved := newRuntime(&TokenSet{AccessToken: "live-token", Issuer: "https://other.example", ClientID: "operator-client", Expiry: time.Now().Add(time.Hour)})
	if _, err := moved.TokenSource(ctx); err == nil {
		t.Fatal("issuer drift accepted")
	}
}

func TestOAuthTokenStoreEnforcesPrivacy(t *testing.T) {
	store, root := testOAuthStore(t)
	ctx := context.Background()
	tokens := TokenSet{AccessToken: "secret", Issuer: "https://example.com", ClientID: "cid"}
	if err := store.SaveTokens(ctx, tokens); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"oauthsrv.json"} {
		info, err := os.Stat(filepath.Join(root, tokenDirName, file))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("token file perms = %o", info.Mode().Perm())
		}
	}
	dirInfo, err := os.Stat(filepath.Join(root, tokenDirName))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm()&0077 != 0 {
		t.Fatalf("token dir perms = %o", dirInfo.Mode().Perm())
	}
	loaded, ok, err := store.LoadTokens(ctx)
	if err != nil || !ok || loaded.AccessToken != "secret" {
		t.Fatalf("tokens = %+v %t %v", loaded, ok, err)
	}
	if _, ok, err := store.LoadClient(ctx); err != nil || ok {
		t.Fatalf("unexpected client = %t %v", ok, err)
	}
}
