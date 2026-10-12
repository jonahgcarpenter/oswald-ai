package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestChallengeParser(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		want   string
	}{
		"quoted multi-scope":   {`Bearer realm="example", scope="read write"`, "read write"},
		"case insensitive key": {`bearer SCOPE="a"`, "a"},
		"unquoted token":       {`Bearer scope=read`, "read"},
		"token68 ignored":      {`Bearer mF_9.B5f-4.1JqM`, ""},
		"absent":               {`Bearer realm="example"`, ""},
		"garbage":              {`not a challenge at all`, ""},
		"empty":                {``, ""},
		"escaped quote":        {`Bearer scope="a \"b\" c"`, `a "b" c`},
		"comma inside quotes":  {`Bearer title="x, y", scope="p q"`, "p q"},
		"first scope wins":     {`Bearer scope="one", scope="two"`, "one"},
		"scheme prefix spaced": {`Bearer  realm="x" ,  scope = "s1 s2"`, "s1 s2"},
		"basic tolerant":       {`Basic realm="x", scope="legacy"`, "legacy"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := parseWWWAuthenticateScope(tc.header); got != tc.want {
				t.Fatalf("scope = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractChallengeScopeMultiHeader(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("WWW-Authenticate", `Bearer realm="example"`)
	resp.Header.Add("WWW-Authenticate", `Bearer scope="y z"`)
	if got := extractChallengeScope(resp); got != "y z" {
		t.Fatalf("scope = %q, want %q", got, "y z")
	}
	if got := parseWWWAuthenticateField(resp, "ERROR"); got != "" {
		t.Fatalf("error = %q, want empty", got)
	}
	errResp := &http.Response{Header: http.Header{}}
	errResp.Header.Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="b"`)
	if got := parseWWWAuthenticateField(errResp, "error"); got != "insufficient_scope" {
		t.Fatalf("error = %q, want insufficient_scope", got)
	}
	var nilResp *http.Response
	if got := extractChallengeScope(nilResp); got != "" {
		t.Fatalf("nil scope = %q, want empty", got)
	}
	if got := parseWWWAuthenticateField(nilResp, "scope"); got != "" {
		t.Fatalf("nil field = %q, want empty", got)
	}
}

func TestUnionScopes(t *testing.T) {
	for name, tc := range map[string]struct {
		previous  string
		challenge string
		want      string
	}{
		"append new":   {"a b", "b c", "a b c"},
		"both empty":   {"", "", ""},
		"prev only":    {"a", "", "a"},
		"chall only":   {"", "b", "b"},
		"exact dup":    {"a b", "a b", "a b"},
		"whitespace":   {"  a\tb ", "  c  ", "a b c"},
		"stored first": {"z a", "m z", "z a m"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := unionScopes(tc.previous, tc.challenge); got != tc.want {
				t.Fatalf("union = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectEffectiveScopesPriority(t *testing.T) {
	meta := func() *serverMetadata {
		return &serverMetadata{
			PRMScopesSupported: []string{"p1", "p2"},
			ScopesSupported:    []string{"s1"},
		}
	}
	for name, tc := range map[string]struct {
		requested []string
		challenge string
		meta      *serverMetadata
		want      []string
	}{
		"config wins verbatim": {[]string{"z"}, "a", meta(), []string{"z"}},
		"challenge beats prm":  {nil, "a b", meta(), []string{"a", "b"}},
		"prm beats as":         {nil, "", meta(), []string{"p1", "p2"}},
		"as fallback":          {nil, "", &serverMetadata{ScopesSupported: []string{"s1"}}, []string{"s1"}},
		"none omitted":         {nil, "", &serverMetadata{}, nil},
		"nil meta omitted":     {nil, "", nil, nil},
		"blank challenge skip": {nil, "   ", meta(), []string{"p1", "p2"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := selectEffectiveScopes(tc.requested, tc.challenge, tc.meta)
			if len(got) != len(tc.want) {
				t.Fatalf("scopes = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("scopes = %q, want %q", got, tc.want)
				}
			}
			// Selection copies: mutating the result must not alias inputs.
			for i := range got {
				got[i] = "mutated"
			}
			if len(tc.requested) > 0 && tc.requested[0] == "mutated" {
				t.Fatal("selection aliases configured scopes")
			}
		})
	}
}

func TestOfflineAccessAppended(t *testing.T) {
	withOffline := &serverMetadata{ScopesSupported: []string{"a", "offline_access"}}
	without := &serverMetadata{ScopesSupported: []string{"a"}}
	if got := withOfflineAccess([]string{"a"}, withOffline); len(got) != 2 || got[1] != "offline_access" {
		t.Fatalf("appended = %q", got)
	}
	if got := withOfflineAccess([]string{"a", "offline_access"}, withOffline); len(got) != 2 {
		t.Fatalf("dup appended = %q", got)
	}
	if got := withOfflineAccess(nil, withOffline); len(got) != 0 {
		t.Fatalf("empty appended = %q", got)
	}
	if got := withOfflineAccess([]string{"a"}, without); len(got) != 1 {
		t.Fatalf("unadvertised appended = %q", got)
	}
	if got := withOfflineAccess([]string{"a"}, nil); len(got) != 1 {
		t.Fatalf("nil meta changed = %q", got)
	}
	if needsOfflineAccess(nil, withOffline) || !needsOfflineAccess([]string{"a"}, withOffline) || needsOfflineAccess([]string{"offline_access"}, withOffline) {
		t.Fatal("needsOfflineAccess matrix wrong")
	}
	// Configured scopes still get the append when advertised.
	if got := selectEffectiveScopes([]string{"z"}, "", withOffline); len(got) != 2 || got[1] != "offline_access" {
		t.Fatalf("config+offline = %q", got)
	}
}

// captureDCR runs performDCR against a registration endpoint that records
// the raw JSON payload, and returns the decoded top-level map.
func captureDCR(t *testing.T, scopes []string) map[string]any {
	t.Helper()
	var raw json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxMetadataBytes+1))
		raw = append([]byte(nil), body...)
		writeJSON(w, map[string]any{"client_id": "dcr-client", "redirect_uris": []string{"http://127.0.0.1:9/callback"}, "token_endpoint_auth_method": "none"})
	}))
	defer server.Close()
	store, _ := testOAuthStore(t)
	meta := &serverMetadata{Issuer: server.URL, RegistrationEndpoint: server.URL + "/register"}
	if _, err := performDCR(context.Background(), store, meta, "http://127.0.0.1:9/callback", scopes); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestScopeOmissionOnWire(t *testing.T) {
	// DCR omits scope via omitempty: the key must be absent, not empty.
	if payload := captureDCR(t, nil); payload["scope"] != nil {
		if _, present := payload["scope"]; present {
			t.Fatalf("DCR payload carries scope key: %v", payload["scope"])
		}
	}
	// Authorize URL omits scope when the selection is empty.
	client := &resolvedOAuthClient{ClientID: "cid", AuthStyle: oauth2.AuthStyleInParams}
	meta := &serverMetadata{AuthorizationEndpoint: "https://example.com/authorize", TokenEndpoint: "https://example.com/token"}
	parsed, err := url.Parse(oauth2Config(client, meta, nil).AuthCodeURL("state", oauth2.S256ChallengeOption(oauth2.GenerateVerifier())))
	if err != nil {
		t.Fatal(err)
	}
	if _, present := parsed.Query()["scope"]; present {
		t.Fatalf("authorize URL carries scope: %q", parsed.RawQuery)
	}
	// Refresh omits scope when neither config nor stored scopes exist.
	var sawScope bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, sawScope = r.PostForm["scope"]
		writeJSON(w, map[string]any{"access_token": "new-access", "token_type": "Bearer", "refresh_token": "rt", "expires_in": 3600})
	}))
	defer server.Close()
	store, _ := testOAuthStore(t)
	ctx := context.Background()
	if err := store.SaveTokens(ctx, TokenSet{AccessToken: "old", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour), Issuer: "http://ha.local", ClientID: "cid"}); err != nil {
		t.Fatal(err)
	}
	refreshMeta := &serverMetadata{Issuer: "http://ha.local", TokenEndpoint: server.URL}
	refreshClient := &resolvedOAuthClient{ClientID: "cid", Method: clientMethodDCR, AuthStyle: oauth2.AuthStyleInParams}
	if _, err := refreshOAuthToken(ctx, store, refreshMeta, refreshClient, nil, "", "", server.Client()); err != nil {
		t.Fatal(err)
	}
	if sawScope {
		t.Fatal("refresh form carries scope key for empty selection")
	}
	// Device omission is covered by TestDeviceOmitsEmptyScopeAndPersistsGranted.
}

func TestScopePriorityOnWire(t *testing.T) {
	meta := &serverMetadata{
		PRMScopesSupported: []string{"prm-a", "prm-b"},
		ScopesSupported:    []string{"as-a"},
	}
	// Challenge beats PRM/AS on the DCR wire.
	eff := selectEffectiveScopes(nil, "live-a live-b", meta)
	if payload := captureDCR(t, eff); payload["scope"] != "live-a live-b" {
		t.Fatalf("DCR scope = %v, want live challenge", payload["scope"])
	}
	// PRM beats AS.
	eff = selectEffectiveScopes(nil, "", meta)
	if payload := captureDCR(t, eff); payload["scope"] != "prm-a prm-b" {
		t.Fatalf("DCR scope = %v, want PRM set", payload["scope"])
	}
	// Authorize URL carries the same selection.
	client := &resolvedOAuthClient{ClientID: "cid", AuthStyle: oauth2.AuthStyleInParams}
	wireMeta := &serverMetadata{AuthorizationEndpoint: "https://example.com/authorize", TokenEndpoint: "https://example.com/token"}
	parsed, err := url.Parse(oauth2Config(client, wireMeta, eff).AuthCodeURL("state"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("scope") != "prm-a prm-b" {
		t.Fatalf("authorize scope = %q", parsed.Query().Get("scope"))
	}
}

func TestConfiguredScopesWin(t *testing.T) {
	meta := &serverMetadata{
		PRMScopesSupported: []string{"prm-a"},
		ScopesSupported:    []string{"as-a"},
	}
	eff := selectEffectiveScopes([]string{"z"}, "live", meta)
	if len(eff) != 1 || eff[0] != "z" {
		t.Fatalf("effective = %q, want [z]", eff)
	}
	if payload := captureDCR(t, eff); payload["scope"] != "z" {
		t.Fatalf("DCR scope = %v, want configured", payload["scope"])
	}
}

func TestStepUpUnionOnCLI(t *testing.T) {
	meta := &serverMetadata{ScopesSupported: []string{"as-a"}}
	// Stored grant plus live challenge unions, stored order first.
	eff := cliEffectiveScopes(nil, []string{"a"}, "b c", meta)
	if len(eff) != 3 || eff[0] != "a" || eff[1] != "b" || eff[2] != "c" {
		t.Fatalf("union = %q, want [a b c]", eff)
	}
	// The union is what registration asks for.
	if payload := captureDCR(t, eff); payload["scope"] != "a b c" {
		t.Fatalf("DCR scope = %v, want union", payload["scope"])
	}
	// Covered challenges do not widen the request.
	if eff := cliEffectiveScopes(nil, []string{"a", "b"}, "a", meta); len(eff) != 2 {
		t.Fatalf("covered union = %q, want [a b]", eff)
	}
	// Configured scopes bypass the union entirely.
	if eff := cliEffectiveScopes([]string{"z"}, []string{"a"}, "b c", meta); len(eff) != 1 || eff[0] != "z" {
		t.Fatalf("configured union = %q, want [z]", eff)
	}
	// No stored grant: plain challenge selection.
	if eff := cliEffectiveScopes(nil, nil, "b c", meta); len(eff) != 2 || eff[0] != "b" {
		t.Fatalf("fresh challenge = %q, want [b c]", eff)
	}
}

func TestGrantedScopeAuthoritativeOnRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The token response scope wins over requested/stored scopes.
		writeJSON(w, map[string]any{"access_token": "new-access", "token_type": "Bearer", "refresh_token": "rt", "expires_in": 3600, "scope": "x y"})
	}))
	defer server.Close()
	store, _ := testOAuthStore(t)
	ctx := context.Background()
	stored := TokenSet{AccessToken: "old", RefreshToken: "rt", Expiry: time.Now().Add(-time.Hour), Issuer: "http://ha.local", ClientID: "cid", Scopes: []string{"requested"}}
	if err := store.SaveTokens(ctx, stored); err != nil {
		t.Fatal(err)
	}
	meta := &serverMetadata{Issuer: "http://ha.local", TokenEndpoint: server.URL}
	client := &resolvedOAuthClient{ClientID: "cid", Method: clientMethodDCR, AuthStyle: oauth2.AuthStyleInParams}
	refreshed, err := refreshOAuthToken(ctx, store, meta, client, []string{"requested"}, "", "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed.Scopes) != 2 || refreshed.Scopes[0] != "x" || refreshed.Scopes[1] != "y" {
		t.Fatalf("scopes = %q, want granted set", refreshed.Scopes)
	}
}

func TestFetchChallengeHarvest(t *testing.T) {
	fake := newFakeAuthorizationServer(t, true)
	fake.challengeScope = "read write"
	defer fake.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	scope, resourceURL := fetchChallenge(ctx, discoveryClient(), fake.mcpURL())
	if scope != "read write" {
		t.Fatalf("challenge scope = %q", scope)
	}
	if resourceURL != fake.server.URL+"/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("resource metadata URL = %q", resourceURL)
	}
	// No challenge configured: fallthrough, no failure.
	plain := newFakeAuthorizationServer(t, true)
	defer plain.close()
	if scope, resourceURL := fetchChallenge(ctx, discoveryClient(), plain.mcpURL()); scope != "" || resourceURL != "" {
		t.Fatalf("plain harvest = %q %q, want empty", scope, resourceURL)
	}
	// Invalid URL: silent fallthrough.
	if scope, resourceURL := fetchChallenge(ctx, nil, "://bad"); scope != "" || resourceURL != "" {
		t.Fatalf("bad-URL harvest = %q %q, want empty", scope, resourceURL)
	}
}

func TestHarvestedResourceMetadataPreferred(t *testing.T) {
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+issuer+`/custom-prm", scope="live"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/custom-prm":
			writeJSON(w, map[string]any{
				"resource": issuer + "/mcp", "authorization_servers": []string{issuer},
				"scopes_supported": []string{"harvested"},
			})
		case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
			writeJSON(w, map[string]any{
				"resource": issuer + "/mcp", "authorization_servers": []string{issuer},
				"scopes_supported": []string{"standard"},
			})
		case "/.well-known/oauth-authorization-server":
			writeJSON(w, map[string]any{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": issuer + "/token",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	scope, harvested := fetchChallenge(ctx, discoveryClient(), issuer+"/mcp")
	if scope != "live" || harvested != issuer+"/custom-prm" {
		t.Fatalf("harvest = %q %q", scope, harvested)
	}
	meta, err := discoverOAuthMetadataWithChallenge(ctx, issuer+"/mcp", discoveryClient(), harvested)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.PRMScopesSupported) != 1 || meta.PRMScopesSupported[0] != "harvested" {
		t.Fatalf("PRM scopes = %q, want harvested doc", meta.PRMScopesSupported)
	}
	if eff := selectEffectiveScopes(nil, "", meta); len(eff) != 1 || eff[0] != "harvested" {
		t.Fatalf("effective = %q, want harvested", eff)
	}
	// A dead harvested URL falls back to the derived candidates.
	meta, err = discoverOAuthMetadataWithChallenge(ctx, issuer+"/mcp", discoveryClient(), issuer+"/missing-prm")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.PRMScopesSupported) != 1 || meta.PRMScopesSupported[0] != "standard" {
		t.Fatalf("fallback PRM scopes = %q, want standard doc", meta.PRMScopesSupported)
	}
}

func TestChallengeSelectionEndToEnd(t *testing.T) {
	// Challenge scope flows through discovery, selection, DCR, the
	// authorize URL, and the granted-scope fallback without configuration.
	fake := newFakeAuthorizationServer(t, false)
	fake.challengeScope = "chat read"
	fake.prmScopes = []string{"prm-a"}
	fake.asScopes = []string{"as-a"}
	defer fake.close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	scope, harvested := fetchChallenge(ctx, discoveryClient(), fake.mcpURL())
	if scope != "chat read" {
		t.Fatalf("challenge scope = %q", scope)
	}
	meta, err := discoverOAuthMetadataWithChallenge(ctx, fake.mcpURL(), discoveryClient(), harvested)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.PRMScopesSupported) != 1 || meta.PRMScopesSupported[0] != "prm-a" {
		t.Fatalf("PRM scopes = %q", meta.PRMScopesSupported)
	}
	store, _ := testOAuthStore(t)
	tokens := driveBrowserFlow(t, ctx, store, &OAuthConfig{Flow: "browser", Timeout: time.Minute}, meta, fake.mcpURL())
	if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "prm-a" {
		t.Fatalf("granted scopes = %q, want PRM fallback", tokens.Scopes)
	}
	// Drive again with the harvested challenge wired through the flow: the
	// fake token endpoint returns no scope, so the granted set falls back
	// to the challenge selection.
	store2, _ := testOAuthStore(t)
	var printed syncBuffer
	done := make(chan struct{})
	var flowErr error
	var challenged *TokenSet
	go func() {
		defer close(done)
		oauth := &OAuthConfig{Flow: "browser", Timeout: time.Minute}
		challenged, flowErr = RunBrowserFlow(ctx, store2, oauth, meta, fake.mcpURL(), FlowOptions{NoBrowser: true, Stdout: &printed, ChallengeScope: scope})
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
			if !strings.Contains(line, "scope=chat") {
				t.Fatalf("authorize URL missing challenge scope: %q", line)
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
	if len(challenged.Scopes) != 2 || challenged.Scopes[0] != "chat" || challenged.Scopes[1] != "read" {
		t.Fatalf("granted scopes = %q, want challenge fallback", challenged.Scopes)
	}
}

func TestDeviceFlowSendsSelectedScope(t *testing.T) {
	fake := newFakeAuthorizationServer(t, false)
	fake.deviceOKAt = 1
	defer fake.close()
	store, _ := testOAuthStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	meta, err := discoverOAuthMetadata(ctx, fake.mcpURL())
	if err != nil {
		t.Fatal(err)
	}
	meta.PRMScopesSupported = []string{"device-scope"}
	tokens, err := RunDeviceFlow(ctx, store, &OAuthConfig{Flow: "device", Timeout: time.Minute}, meta, fake.mcpURL(), FlowOptions{Stdout: &syncBuffer{}})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	sent, present := fake.lastDeviceScope, fake.lastDeviceScopeSet
	fake.mu.Unlock()
	if !present || sent != "device-scope" {
		t.Fatalf("device scope = %q present=%t, want device-scope", sent, present)
	}
	if len(tokens.Scopes) != 1 || tokens.Scopes[0] != "device-scope" {
		t.Fatalf("granted scopes = %q, want selection fallback", tokens.Scopes)
	}
}

func TestAuthorizeStepUpFailsClosed(t *testing.T) {
	newRuntime := func(t *testing.T, ctx context.Context, tokens TokenSet) *oauthRuntime {
		t.Helper()
		cfg := ServerConfig{Name: "oauthsrv", Transport: TransportStreamableHTTP, URL: "http://ha.local/mcp", OAuth: &OAuthConfig{Flow: "browser"}, Enabled: true}
		runtime, err := newOAuthRuntime(t.TempDir(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		meta := serverMetadata{Issuer: "http://ha.local", AuthorizationEndpoint: "http://ha.local/authorize", TokenEndpoint: "http://ha.local/token"}
		if err := runtime.store.SaveMetadata(ctx, meta); err != nil {
			t.Fatal(err)
		}
		if err := runtime.store.SaveTokens(ctx, tokens); err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	stored := TokenSet{AccessToken: "live", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour), Issuer: "http://ha.local", ClientID: "cid", Scopes: []string{"a"}}
	challenge := func(header string) *http.Response {
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody, Header: http.Header{"Www-Authenticate": []string{header}}}
	}
	// Uncovered insufficient_scope fails closed into a re-login prompt.
	ctx := context.Background()
	runtime := newRuntime(t, ctx, stored)
	err := runtime.Authorize(ctx, &http.Request{Header: http.Header{"Authorization": []string{"Bearer live"}}}, challenge(`Bearer error="insufficient_scope", scope="b c"`))
	if err == nil || !strings.Contains(err.Error(), "oswald mcp auth") {
		t.Fatalf("step-up accepted or wrong error: %v", err)
	}
	// Covered scope keeps the existing peer-dedup path: a mismatched
	// bearer means a peer already rotated, so Authorize returns nil.
	runtime = newRuntime(t, ctx, stored)
	if err := runtime.Authorize(ctx, &http.Request{Header: http.Header{"Authorization": []string{"Bearer stale-peer"}}}, challenge(`Bearer error="insufficient_scope", scope="a"`)); err != nil {
		t.Fatalf("covered step-up failed: %v", err)
	}
	// A non-step-up 401 keeps the refresh path: mismatched bearer still
	// dedups without network use.
	runtime = newRuntime(t, ctx, stored)
	if err := runtime.Authorize(ctx, &http.Request{Header: http.Header{"Authorization": []string{"Bearer stale-peer"}}}, challenge(`Bearer realm="x"`)); err != nil {
		t.Fatalf("plain challenge failed: %v", err)
	}
}
