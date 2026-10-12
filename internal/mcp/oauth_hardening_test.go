package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// driveBrowserFlow runs one browser attempt to completion by following every
// printed authorization URL (which redirects back to the loopback listener).
func driveBrowserFlow(t *testing.T, ctx context.Context, store *tokenStore, oauth *OAuthConfig, meta *serverMetadata, mcpURL string) *TokenSet {
	t.Helper()
	var printed syncBuffer
	done := make(chan struct{})
	var flowErr error
	var tokens *TokenSet
	go func() {
		defer close(done)
		tokens, flowErr = RunBrowserFlow(ctx, store, oauth, meta, mcpURL, FlowOptions{NoBrowser: true, Stdout: &printed})
	}()
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
	return tokens
}

func TestIssuerTrailingSlashNormalized(t *testing.T) {
	if !issuerMatches("https://host.example/", "https://host.example") {
		t.Fatal("trailing-slash issuer mismatch")
	}
	if issuerMatches("https://host.example", "https://other.example") {
		t.Fatal("cross-origin issuer accepted")
	}
}

func TestOriginIssuedMetadataAccepted(t *testing.T) {
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			writeJSON(w, map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer + "/sub"}})
		case "/.well-known/oauth-authorization-server/sub":
			writeJSON(w, map[string]any{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint":                   issuer + "/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	meta, err := discoverOAuthMetadata(context.Background(), server.URL+"/mcp")
	if err != nil {
		t.Fatalf("origin-issued metadata rejected: %v", err)
	}
	if meta.Issuer != server.URL+"/sub" {
		t.Fatalf("advertised issuer not retained: %+v", meta)
	}
}

func TestForeignIssuedMetadataRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "oauth-protected-resource"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "oauth-authorization-server"):
			writeJSON(w, map[string]any{
				"issuer": "https://other.example", "authorization_endpoint": "https://other.example/authorize",
				"token_endpoint":                   "https://other.example/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if _, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", oauthMappedClient(server)); err == nil {
		t.Fatal("foreign issuer accepted")
	}
}

func TestDiscoverySendsProductUserAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "oswald-ai/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if strings.Contains(r.URL.Path, "oauth-protected-resource") {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"issuer": "http://ha.local", "authorization_endpoint": "http://ha.local/authorize",
			"token_endpoint":                   "http://ha.local/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	}))
	mappedUA := &http.Client{Transport: oauthTestTransport(func(r *http.Request) (*http.Response, error) {
		mapped := r.Clone(r.Context())
		if strings.TrimSpace(mapped.Header.Get("User-Agent")) == "" {
			mapped.Header.Set("User-Agent", oauthUserAgent)
		}
		u := *r.URL
		u.Scheme = "http"
		u.Host = strings.TrimPrefix(server.URL, "http://")
		mapped.URL = &u
		return http.DefaultTransport.RoundTrip(mapped)
	})}
	if _, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", mappedUA); err != nil {
		t.Fatalf("product UA rejected: %v", err)
	}
	if _, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", oauthMappedClient(server)); err == nil {
		t.Fatal("generic client passed a WAF gate")
	}
}

func TestS256Matrix(t *testing.T) {
	discover := func(t *testing.T, methods any) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "oauth-protected-resource") {
				http.NotFound(w, r)
				return
			}
			doc := map[string]any{
				"issuer": "http://ha.local", "authorization_endpoint": "http://ha.local/authorize",
				"token_endpoint": "http://ha.local/token",
			}
			if methods != nil {
				doc["code_challenge_methods_supported"] = methods
			}
			writeJSON(w, doc)
		}))
		defer server.Close()
		_, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", oauthMappedClient(server))
		return err
	}
	if err := discover(t, nil); err != nil {
		t.Fatalf("unadvertised S256 rejected: %v", err)
	}
	if err := discover(t, []string{"S256"}); err != nil {
		t.Fatalf("advertised S256 rejected: %v", err)
	}
	err := discover(t, []string{"plain"})
	if err == nil || !strings.Contains(err.Error(), "plain") {
		t.Fatalf("plain-only accepted or advertised values hidden: %v", err)
	}
}

func TestMetadataContentTypeMatrix(t *testing.T) {
	discover := func(t *testing.T, contentType string) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "oauth-protected-resource") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write([]byte(`{"issuer":"http://ha.local","authorization_endpoint":"http://ha.local/authorize","token_endpoint":"http://ha.local/token","code_challenge_methods_supported":["S256"]}`))
		}))
		defer server.Close()
		_, err := discoverOAuthMetadataWithClient(context.Background(), "http://ha.local/mcp", oauthMappedClient(server))
		return err
	}
	if err := discover(t, "application/json; charset=utf-8"); err != nil {
		t.Fatalf("charset JSON rejected: %v", err)
	}
	if err := discover(t, "application/oauth-metadata+json"); err != nil {
		t.Fatalf("suffix JSON rejected: %v", err)
	}
	if err := discover(t, "text/html"); err == nil {
		t.Fatal("HTML metadata accepted")
	}
}

func TestDCRSecretWithoutMethodUsesBody(t *testing.T) {
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/register":
			writeJSON(w, map[string]any{"client_id": "secret-client", "client_secret": "shh", "redirect_uris": []string{"http://127.0.0.1:9/callback"}})
		case "/token":
			_ = r.ParseForm()
			// client_secret_post puts the secret in the body, not basic auth.
			if _, _, ok := r.BasicAuth(); ok {
				writeOAuthError(w, "invalid_client")
				return
			}
			if r.PostForm.Get("client_id") != "secret-client" || r.PostForm.Get("client_secret") != "shh" {
				writeOAuthError(w, "invalid_client")
				return
			}
			writeJSON(w, map[string]any{"access_token": "ok", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	store, _ := testOAuthStore(t)
	meta := &serverMetadata{Issuer: issuer, TokenEndpoint: issuer + "/token", RegistrationEndpoint: issuer + "/register"}
	client, err := performDCR(context.Background(), store, meta, "http://127.0.0.1:9/callback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.AuthStyle != oauth2.AuthStyleInParams {
		t.Fatalf("auth style = %v, want body", client.AuthStyle)
	}
	if _, err := exchangeCode(context.Background(), client, meta, nil, issuer+"/mcp", "code", "verifier", server.Client()); err != nil {
		t.Fatalf("exchange with coerced body auth failed: %v", err)
	}
}

func TestRefreshCarriesForwardMissingFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RFC 6749 §6: rotation and scope are optional on refresh.
		writeJSON(w, map[string]any{"access_token": "new-access", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer server.Close()
	store, _ := testOAuthStore(t)
	ctx := context.Background()
	stored := TokenSet{AccessToken: "old", RefreshToken: "keep-me", Expiry: time.Now().Add(-time.Hour), Issuer: "http://ha.local", ClientID: "cid", Scopes: []string{"read", "write"}}
	if err := store.SaveTokens(ctx, stored); err != nil {
		t.Fatal(err)
	}
	meta := &serverMetadata{Issuer: "http://ha.local", TokenEndpoint: "http://ha.local/token"}
	client := &resolvedOAuthClient{ClientID: "cid", Method: clientMethodDCR, AuthStyle: oauth2.AuthStyleInParams}
	refreshed, err := refreshOAuthToken(ctx, store, meta, client, nil, "http://ha.local/mcp", "", oauthMappedClient(server))
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken != "keep-me" {
		t.Fatalf("refresh token dropped: %+v", refreshed)
	}
	if len(refreshed.Scopes) != 2 || refreshed.Scopes[0] != "read" {
		t.Fatalf("scopes dropped: %+v", refreshed)
	}
}

func TestRefreshSendsResourceIndicator(t *testing.T) {
	const resource = "http://ha.local/mcp"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("resource") != resource {
			writeOAuthError(w, "invalid_target")
			return
		}
		writeJSON(w, map[string]any{"access_token": "new-access", "token_type": "Bearer", "refresh_token": "new-refresh", "expires_in": 3600})
	}))
	defer server.Close()
	store, _ := testOAuthStore(t)
	ctx := context.Background()
	if err := store.SaveTokens(ctx, TokenSet{AccessToken: "old", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour), Issuer: "http://ha.local", ClientID: "cid"}); err != nil {
		t.Fatal(err)
	}
	meta := &serverMetadata{Issuer: "http://ha.local", TokenEndpoint: "http://ha.local/token"}
	client := &resolvedOAuthClient{ClientID: "cid", Method: clientMethodDCR, AuthStyle: oauth2.AuthStyleInParams}
	if _, err := refreshOAuthToken(ctx, store, meta, client, nil, resource, "", oauthMappedClient(server)); err != nil {
		t.Fatalf("resource-bound refresh failed: %v", err)
	}
}

func TestSingleUseRefreshParallelCallers(t *testing.T) {
	// Single-use grants: the refresh token rotates on first use, so a peer
	// that arrives late must adopt the winner instead of POSTing the burned
	// grant. Exactly one POST carries the original refresh token.
	const issuer = "http://single.use"
	var mu sync.Mutex
	current := "rt-1"
	var burnedPosts int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		if r.PostForm.Get("refresh_token") != current {
			atomic.AddInt64(&burnedPosts, 1)
			writeOAuthError(w, "invalid_grant")
			return
		}
		current = "rt-2"
		writeJSON(w, map[string]any{"access_token": "rotated-once", "token_type": "Bearer", "refresh_token": "rt-2", "expires_in": 3600})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	meta := &serverMetadata{Issuer: issuer, TokenEndpoint: server.URL}
	root := t.TempDir()
	mkRuntime := func() *oauthRuntime {
		t.Helper()
		cfg := ServerConfig{Name: "oauthsrv", Transport: TransportStreamableHTTP, URL: issuer + "/mcp", OAuth: &OAuthConfig{Flow: "browser"}, Enabled: true}
		runtime, err := newOAuthRuntime(root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.store.SaveMetadata(ctx, *meta); err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	first := mkRuntime()
	if err := first.store.SaveTokens(ctx, TokenSet{AccessToken: "stale", RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Hour), Issuer: issuer, ClientID: "cid"}); err != nil {
		t.Fatal(err)
	}
	second := mkRuntime()
	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i, rt := range []*oauthRuntime{first, second} {
		wg.Add(1)
		go func(i int, rt *oauthRuntime) {
			defer wg.Done()
			source, err := rt.TokenSource(ctx)
			if err != nil {
				errs[i] = err
				return
			}
			token, err := source.Token()
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = token.AccessToken
		}(i, rt)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i] != "rotated-once" {
			t.Fatalf("caller %d ended with %q", i, results[i])
		}
	}
	if got := atomic.LoadInt64(&burnedPosts); got != 0 {
		t.Fatalf("burned-grant POSTs = %d, want 0", got)
	}
}

func TestDeadDynamicClientDiscarded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		writeOAuthError(w, "invalid_client")
	}))
	defer server.Close()
	ctx := context.Background()
	meta := &serverMetadata{Issuer: "http://ha.local", TokenEndpoint: "http://ha.local/token", RegistrationEndpoint: "http://ha.local/register"}
	mkRuntime := func(oauth *OAuthConfig, client *ClientRecord, tokens TokenSet) *oauthRuntime {
		t.Helper()
		cfg := ServerConfig{Name: "oauthsrv", Transport: TransportStreamableHTTP, URL: "http://ha.local/mcp", OAuth: oauth, Enabled: true}
		runtime, err := newOAuthRuntime(t.TempDir(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.store.SaveMetadata(ctx, *meta); err != nil {
			t.Fatal(err)
		}
		if client != nil {
			if err := runtime.store.SaveClient(ctx, *client); err != nil {
				t.Fatal(err)
			}
		}
		if err := runtime.store.SaveTokens(ctx, tokens); err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	dead := TokenSet{AccessToken: "stale", RefreshToken: "dead-grant", Expiry: time.Now().Add(-time.Hour), Issuer: "http://ha.local", ClientID: "dcr-client"}
	dynamic := mkRuntime(&OAuthConfig{Flow: "browser"}, &ClientRecord{ClientID: "dcr-client", RedirectURIs: []string{"http://127.0.0.1:9/callback"}, Method: clientMethodDCR}, dead)
	// Point the runtime at the failing token endpoint. The issuer stays
	// http://ha.local so the failure lands on the grant, not drift.
	metaFail := &serverMetadata{Issuer: "http://ha.local", TokenEndpoint: server.URL + "/token", RegistrationEndpoint: server.URL + "/register"}
	if err := dynamic.store.SaveMetadata(ctx, *metaFail); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamic.TokenSource(ctx); err == nil {
		t.Fatal("dead dynamic grant accepted")
	}
	if _, ok, _ := dynamic.store.LoadClient(ctx); ok {
		t.Fatal("dead dynamic registration retained")
	}
	if _, ok, _ := dynamic.store.LoadTokens(ctx); ok {
		t.Fatal("dead dynamic tokens retained")
	}
	// Operator clients never auto-heal: material is kept for inspection.
	operator := mkRuntime(&OAuthConfig{Flow: "browser", ClientID: "operator-client"}, nil, TokenSet{AccessToken: "stale", RefreshToken: "dead-grant", Expiry: time.Now().Add(-time.Hour), Issuer: "http://ha.local", ClientID: "operator-client"})
	if err := operator.store.SaveMetadata(ctx, *metaFail); err != nil {
		t.Fatal(err)
	}
	if _, err := operator.TokenSource(ctx); err == nil {
		t.Fatal("dead operator grant accepted")
	}
	if _, ok, _ := operator.store.LoadTokens(ctx); !ok {
		t.Fatal("operator tokens auto-discarded")
	}
}

func TestReloginReusesCachedRedirect(t *testing.T) {
	fake := newFakeAuthorizationServer(t, false)
	defer fake.close()
	// Count registrations: the fake always issues dcr-client.
	var registrations int64
	_ = registrations
	store, _ := testOAuthStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	oauth := &OAuthConfig{Flow: "browser", Timeout: time.Minute}
	first := driveBrowserFlow(t, ctx, store, oauth, meta, fake.mcpURL())
	record, ok, err := store.LoadClient(ctx)
	if err != nil || !ok {
		t.Fatalf("no cached client after first login: %+v %v", record, err)
	}
	cached := record.RedirectURIs[0]
	second := driveBrowserFlow(t, ctx, store, oauth, meta, fake.mcpURL())
	record2, ok, err := store.LoadClient(ctx)
	if err != nil || !ok || record2.RedirectURIs[0] != cached {
		t.Fatalf("registration leaked: %+v %t %v", record2, ok, err)
	}
	if first.ClientID != "dcr-client" || second.ClientID != "dcr-client" {
		t.Fatalf("clients = %q %q", first.ClientID, second.ClientID)
	}
}

func TestPasteBackSkipExits(t *testing.T) {
	if _, err := parsePastedCode("skip", "state"); err != errAuthSkipped {
		t.Fatalf("skip not honored: %v", err)
	}
	if code, err := parsePastedCode("abc123", "state"); err != nil || code != "abc123" {
		t.Fatalf("raw code rejected: %q %v", code, err)
	}
}

func TestSelectAuthStyleCoercion(t *testing.T) {
	if got := selectAuthStyle(nil, true); got != oauth2.AuthStyleInParams {
		t.Fatalf("missing method with secret = %v", got)
	}
	if got := selectAuthStyle([]string{"none"}, true); got != oauth2.AuthStyleInParams {
		t.Fatalf("none-only with secret = %v", got)
	}
	if got := selectAuthStyle([]string{"client_secret_basic"}, true); got != oauth2.AuthStyleInHeader {
		t.Fatalf("explicit basic not respected: %v", got)
	}
}

func TestScopeSelectionSourceMatrix(t *testing.T) {
	prm := &serverMetadata{PRMScopesSupported: []string{"read", "write"}}
	as := &serverMetadata{ScopesSupported: []string{"internal"}}
	both := &serverMetadata{PRMScopesSupported: []string{"read"}, ScopesSupported: []string{"internal"}}
	for name, tc := range map[string]struct {
		oauth     *OAuthConfig
		challenge string
		meta      *serverMetadata
		want      string
	}{
		"config wins":         {&OAuthConfig{Scopes: []string{"read"}}, "other", both, "config"},
		"challenge beats prm": {&OAuthConfig{}, "live", both, "challenge"},
		"prm beats as":        {&OAuthConfig{}, "", both, "prm"},
		"as fallback":         {&OAuthConfig{}, "", as, "as"},
		"prm only":            {&OAuthConfig{}, "", prm, "prm"},
		"omitted":             {&OAuthConfig{}, "", &serverMetadata{}, "omitted"},
		"nil meta":            {&OAuthConfig{}, "", nil, "omitted"},
	} {
		t.Run(name, func(t *testing.T) {
			var scopes []string
			if tc.oauth != nil {
				scopes = tc.oauth.Scopes
			}
			if got := scopeSelectionSource(scopes, tc.challenge, tc.meta); got != tc.want {
				t.Fatalf("source = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExchangePersistsGrantedScope(t *testing.T) {
	var granted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		resp := map[string]any{"access_token": "access-1", "token_type": "Bearer", "refresh_token": "refresh-1", "expires_in": 3600}
		if granted != "" {
			resp["scope"] = granted
		}
		writeJSON(w, resp)
	}))
	defer server.Close()
	client := &resolvedOAuthClient{ClientID: "cid", AuthStyle: oauth2.AuthStyleInParams}
	meta := &serverMetadata{Issuer: server.URL, TokenEndpoint: server.URL + "/token", AuthorizationEndpoint: server.URL + "/authorize"}
	ctx := context.Background()
	// Granted scope wins over requested scopes.
	granted = "granted-a granted-b"
	tokens, err := exchangeCode(ctx, client, meta, []string{"requested"}, server.URL+"/mcp", "code", "verifier", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens.Scopes) != 2 || tokens.Scopes[0] != "granted-a" || tokens.Scopes[1] != "granted-b" {
		t.Fatalf("scopes = %q, want granted set", tokens.Scopes)
	}
	// Absent scope falls back to requested scopes.
	granted = ""
	tokens, err = exchangeCode(ctx, client, meta, []string{"requested"}, server.URL+"/mcp", "code", "verifier", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "requested" {
		t.Fatalf("scopes = %q, want requested set", tokens.Scopes)
	}
}

func TestDeviceOmitsEmptyScopeAndPersistsGranted(t *testing.T) {
	var mu sync.Mutex
	var seenScope []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/device":
			mu.Lock()
			_, present := r.PostForm["scope"]
			seenScope = append(seenScope, r.PostForm.Get("scope"))
			if present && r.PostForm.Get("scope") == "" {
				// Recorded via sentinel below; empty-but-present is a failure.
				seenScope[len(seenScope)-1] = "<present-empty>"
			}
			mu.Unlock()
			writeJSON(w, map[string]any{
				"device_code": "device-code-1", "user_code": "USER-CODE",
				"verification_uri": "http://example.com/activate", "expires_in": 300, "interval": 0,
			})
		case "/token":
			writeJSON(w, map[string]any{"access_token": "access-device", "token_type": "Bearer", "refresh_token": "refresh-device", "expires_in": 3600, "scope": "granted-dev"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer := server.URL
	meta := &serverMetadata{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", DeviceAuthorizationEndpoint: issuer + "/device"}
	run := func(scopes []string) *TokenSet {
		t.Helper()
		store, _ := testOAuthStore(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		tokens, err := RunDeviceFlow(ctx, store, &OAuthConfig{Flow: "device", Timeout: time.Minute, Scopes: scopes}, meta, issuer+"/mcp", FlowOptions{Stdout: &syncBuffer{}})
		if err != nil {
			t.Fatal(err)
		}
		return tokens
	}
	// Omitted scope is omitted on the wire; granted scope is persisted.
	tokens := run(nil)
	mu.Lock()
	sent := seenScope[len(seenScope)-1]
	mu.Unlock()
	if sent != "" {
		t.Fatalf("device request sent scope %q, want omitted", sent)
	}
	if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "granted-dev" {
		t.Fatalf("scopes = %q, want granted set", tokens.Scopes)
	}
	// Configured scope is sent.
	tokens = run([]string{"wanted"})
	mu.Lock()
	sent = seenScope[len(seenScope)-1]
	mu.Unlock()
	if sent != "wanted" {
		t.Fatalf("device request sent scope %q, want %q", sent, "wanted")
	}
	if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "granted-dev" {
		t.Fatalf("scopes = %q, want granted set", tokens.Scopes)
	}
}
