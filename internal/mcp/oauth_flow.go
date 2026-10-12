package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// FlowOptions drives one interactive OAuth authorization run from the CLI.
// Stdin is only read in paste-back and device modes; nothing secret is ever
// written to stderr.
type FlowOptions struct {
	NoBrowser bool
	Timeout   time.Duration
	Stdin     io.Reader
	Stdout    io.Writer
	HTTP      *http.Client
	// ChallengeScope carries a harvested live-challenge scope (see
	// fetchChallenge) for automatic scope selection. Empty disables the
	// challenge rule; explicit oauth.Scopes always win regardless.
	ChallengeScope string
}

// oauth2Config builds the token-endpoint configuration for a resolved client.
func oauth2Config(client *resolvedOAuthClient, meta *serverMetadata, scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     client.ClientID,
		ClientSecret: client.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   meta.AuthorizationEndpoint,
			TokenURL:  meta.TokenEndpoint,
			AuthStyle: client.AuthStyle,
		},
		RedirectURL: client.RedirectURI,
		Scopes:      scopes,
	}
}

// RunBrowserFlow performs the authorization-code flow with PKCE: it binds a
// loopback callback, opens (or prints) the authorization URL, waits for the
// redirect or a pasted-back response, and exchanges the code for tokens.
// On a CIMD refusal it records the marker, registers dynamically, and runs
// the flow once more.
func RunBrowserFlow(ctx context.Context, store *tokenStore, oauth *OAuthConfig, meta *serverMetadata, mcpURL string, opts FlowOptions) (*TokenSet, error) {
	timeout := oauth.Timeout
	if opts.Timeout > 0 {
		timeout = opts.Timeout
	}
	for attempt := 0; attempt < 2; attempt++ {
		tokens, cimdRefused, err := browserAttempt(ctx, store, oauth, meta, mcpURL, opts, timeout, attempt > 0)
		if err != nil {
			return nil, err
		}
		if !cimdRefused {
			return tokens, nil
		}
		if err := store.DisableCIMD(ctx); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("OAuth authorization failed")
}

func browserAttempt(ctx context.Context, store *tokenStore, oauth *OAuthConfig, meta *serverMetadata, mcpURL string, opts FlowOptions, timeout time.Duration, forceDCR bool) (*TokenSet, bool, error) {
	verifier := oauth2.GenerateVerifier()
	state := rand.Text()
	listener, redirectURI, err := bindCallbackListener(ctx, store, oauth, forceDCR)
	if err != nil {
		return nil, false, err
	}
	defer listener.Close()
	// Automatic scope selection: explicit config wins, then the live
	// challenge, then advertised PRM/AS scopes, else omitted (the server
	// grants its default set). Scope values are never logged.
	eff := selectEffectiveScopes(oauth.Scopes, opts.ChallengeScope, meta)
	client, err := resolveFlowClient(ctx, store, oauth, meta, redirectURI, forceDCR, eff)
	if err != nil {
		return nil, false, err
	}
	authURL := oauth2Config(client, meta, eff).AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", oauthResource(meta, mcpURL)),
	)
	// Start the callback server before printing the URL so there is no
	// steal window between listen and serve.
	calls := make(chan callbackResult, 1)
	server := &http.Server{Handler: callbackHandler(calls, state, meta.Issuer)}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	if !opts.NoBrowser {
		_ = openBrowser(authURL)
	}
	fmt.Fprintf(opts.Stdout, "Open this URL to authorize:\n%s\n", authURL)
	if opts.Stdin != nil {
		if opts.NoBrowser {
			fmt.Fprintln(opts.Stdout, "After approving, paste the full redirect URL or the code here (or type skip to cancel):")
		} else {
			fmt.Fprintln(opts.Stdout, "If the browser does not complete the login (remote/headless session), paste the full redirect URL or the code here (or type skip to cancel):")
		}
	}
	codeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, callbackErr := waitForCode(codeCtx, calls, state, opts)
	if callbackErr != nil {
		return nil, false, callbackErr
	}
	tokens, err := exchangeCode(ctx, client, meta, eff, oauthResource(meta, mcpURL), code, verifier, opts.HTTP)
	if err != nil {
		if client.Method == clientMethodCIMD && isInvalidClient(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return tokens, false, nil
}

// bindCallbackListener reuses a cached dynamic-client redirect URI when this
// run will use it, so re-login does not leak a fresh DCR registration per
// ephemeral port. Otherwise it binds a fresh loopback port.
func bindCallbackListener(ctx context.Context, store *tokenStore, oauth *OAuthConfig, forceDCR bool) (net.Listener, string, error) {
	if !forceDCR && strings.TrimSpace(oauth.ClientID) == "" {
		if cached, ok, err := store.CachedRedirectURI(ctx); err == nil && ok {
			if listener, redirect, ok := listenRedirectURI(cached); ok {
				return listener, redirect, nil
			}
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("bind OAuth callback: %w", err)
	}
	return listener, "http://" + listener.Addr().String() + "/callback", nil
}

// listenRedirectURI binds the exact loopback port of a previously registered
// redirect URI. It reports false when the URI is not a loopback callback or
// the port is unavailable, in which case the caller binds fresh.
func listenRedirectURI(raw string) (net.Listener, string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" {
		return nil, "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" {
		return nil, "", false
	}
	port := parsed.Port()
	if port == "" {
		return nil, "", false
	}
	if parsed.Path != "/callback" && parsed.Path != "/callback/" {
		return nil, "", false
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return nil, "", false
	}
	return listener, "http://127.0.0.1:" + port + "/callback", true
}

type callbackResult struct {
	code string
	err  error
}

// callbackHandler validates one authorization redirect: exact state match,
// optional RFC 9207 iss binding, query-less follow-ups (favicon) ignored so
// they cannot clobber the real response, first terminal result wins.
func callbackHandler(calls chan<- callbackResult, state, issuer string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("code") == "" && query.Get("error") == "" && query.Get("state") == "" {
			_, _ = io.WriteString(w, "Waiting for authorization.")
			return
		}
		fail := func(err error, page string) {
			select {
			case calls <- callbackResult{err: err}:
			default:
			}
			_, _ = io.WriteString(w, page)
		}
		if errText := query.Get("error"); errText != "" {
			fail(fmt.Errorf("authorization server refused: %s", errText), "Authorization failed. You can close this tab.")
			return
		}
		if query.Get("state") != state {
			fail(fmt.Errorf("authorization state mismatch"), "Authorization failed state check. You can close this tab.")
			return
		}
		if iss := query.Get("iss"); iss != "" && !issuerMatches(iss, issuer) {
			if ok, _ := tolerateOriginIssuer(iss, issuer); !ok {
				fail(fmt.Errorf("authorization issuer mismatch"), "Authorization failed issuer check. You can close this tab.")
				return
			}
		}
		if query.Get("code") == "" {
			fail(fmt.Errorf("authorization response has no code"), "Authorization failed. You can close this tab.")
			return
		}
		select {
		case calls <- callbackResult{code: query.Get("code")}:
		default:
		}
		_, _ = io.WriteString(w, "Authorized. You can close this tab.")
	})
}

func resolveFlowClient(ctx context.Context, store *tokenStore, oauth *OAuthConfig, meta *serverMetadata, redirectURI string, forceDCR bool, scopes []string) (*resolvedOAuthClient, error) {
	if !forceDCR {
		if client, err := resolveOAuthClient(ctx, oauth, store, meta, redirectURI); err == nil {
			return client, nil
		} else if err != errNoCIMD {
			return nil, err
		}
	}
	return performDCR(ctx, store, meta, redirectURI, scopes)
}

func waitForCode(ctx context.Context, calls <-chan callbackResult, state string, opts FlowOptions) (string, error) {
	if opts.Stdin != nil {
		pasted := make(chan string, 1)
		go func() {
			line, _ := bufio.NewReader(opts.Stdin).ReadString('\n')
			pasted <- strings.TrimSpace(line)
		}()
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("OAuth authorization timed out")
		case res := <-calls:
			return res.code, res.err
		case line := <-pasted:
			return parsePastedCode(line, state)
		}
	}
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("OAuth authorization timed out")
	case res := <-calls:
		return res.code, res.err
	}
}

// errAuthSkipped reports an operator-cancelled authorization run.
var errAuthSkipped = fmt.Errorf("OAuth authorization skipped")

func parsePastedCode(line, state string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("no authorization response pasted")
	}
	switch strings.ToLower(line) {
	case "skip", "quit", "exit", "cancel":
		return "", errAuthSkipped
	}
	if strings.Contains(line, "://") {
		parsed, err := url.Parse(line)
		if err != nil {
			return "", fmt.Errorf("pasted redirect URL is invalid")
		}
		query := parsed.Query()
		if errText := query.Get("error"); errText != "" {
			return "", fmt.Errorf("authorization server refused: %s", errText)
		}
		if query.Get("state") != "" && query.Get("state") != state {
			return "", fmt.Errorf("authorization state mismatch")
		}
		if query.Get("code") == "" {
			return "", fmt.Errorf("pasted redirect has no code")
		}
		return query.Get("code"), nil
	}
	return line, nil
}

func openBrowser(target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{target}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		command, args = "xdg-open", []string{target}
	}
	return exec.Command(command, args...).Start()
}

func exchangeCode(ctx context.Context, client *resolvedOAuthClient, meta *serverMetadata, scopes []string, resource, code, verifier string, httpClient *http.Client) (*TokenSet, error) {
	cfg := oauth2Config(client, meta, scopes)
	tokenCtx := context.WithValue(ctx, oauth2.HTTPClient, oauthHTTPClient(httpClient))
	token, err := cfg.Exchange(tokenCtx, code,
		oauth2.VerifierOption(verifier),
		oauth2.SetAuthURLParam("resource", resource),
	)
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}
	return tokenSetFromOAuth(token, meta.Issuer, client.ClientID, scopes), nil
}

func isInvalidClient(err error) bool {
	if err == nil {
		return false
	}
	var refreshErr *refreshError
	if errors.As(err, &refreshErr) {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(refreshErr.body, &body) == nil {
			return body.Error == "invalid_client"
		}
		return false
	}
	if retrieveErr, ok := err.(*oauth2.RetrieveError); ok {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(retrieveErr.Body, &body) == nil {
			return body.Error == "invalid_client"
		}
	}
	return strings.Contains(strings.ToLower(err.Error()), "invalid_client")
}

// deviceAuthorizationResponse is the RFC 8628 device authorization payload.
type deviceAuthorizationResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	CompleteURI     string `json:"verification_uri_complete,omitempty"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// RunDeviceFlow performs the RFC 8628 device authorization grant: it prints
// the verification URI and user code, then polls until the operator approves.
func RunDeviceFlow(ctx context.Context, store *tokenStore, oauth *OAuthConfig, meta *serverMetadata, mcpURL string, opts FlowOptions) (*TokenSet, error) {
	if strings.TrimSpace(meta.DeviceAuthorizationEndpoint) == "" {
		return nil, fmt.Errorf("server does not advertise a device authorization endpoint")
	}
	httpClient := oauthHTTPClient(opts.HTTP)
	// Automatic scope selection, as in browserAttempt: the effective scope
	// is used for registration, the device request, the token poll config,
	// and the grant fallback. An omitted scope is omitted on the wire.
	eff := selectEffectiveScopes(oauth.Scopes, opts.ChallengeScope, meta)
	// Device flow needs a client but no redirect URI; resolve with a placeholder
	// loopback URI, then register dynamically when required.
	client, err := resolveOAuthClient(ctx, oauth, store, meta, "http://127.0.0.1:1/callback")
	if err != nil {
		if err != errNoCIMD {
			return nil, err
		}
		if client, err = performDCR(ctx, store, meta, "http://127.0.0.1:1/callback", eff); err != nil {
			return nil, err
		}
	}
	form := url.Values{"client_id": {client.ClientID}}
	// An omitted scope is omitted on the wire: the server grants its default
	// set. Set oauth.scopes for a narrower grant or a server that requires
	// an explicit scope.
	if scope := strings.Join(eff, " "); strings.TrimSpace(scope) != "" {
		form.Set("scope", scope)
	}
	form.Set("resource", oauthResource(meta, mcpURL))
	if client.ClientSecret != "" {
		form.Set("client_secret", client.ClientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.DeviceAuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request device authorization: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("request device authorization: unexpected status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	var device deviceAuthorizationResponse
	if err := json.Unmarshal(data, &device); err != nil || device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		return nil, fmt.Errorf("device authorization response is invalid")
	}
	uri := device.VerificationURI
	if device.CompleteURI != "" {
		uri = device.CompleteURI
	}
	fmt.Fprintf(opts.Stdout, "Open %s and enter code: %s\n", uri, device.UserCode)
	interval := time.Duration(device.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	if device.ExpiresIn <= 0 {
		if timeout := oauth.Timeout; timeout > 0 {
			deadline = time.Now().Add(timeout)
		} else {
			deadline = time.Now().Add(5 * time.Minute)
		}
	}
	cfg := oauth2Config(client, meta, eff)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("device authorization expired")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		token, err := pollDeviceToken(ctx, cfg, httpClient, client, device.DeviceCode, oauthResource(meta, mcpURL))
		if err != nil {
			if isDevicePending(err) {
				continue
			}
			if isDeviceSlowDown(err) {
				interval *= 2
				continue
			}
			return nil, err
		}
		return tokenSetFromOAuth(token, meta.Issuer, client.ClientID, eff), nil
	}
}

func pollDeviceToken(ctx context.Context, cfg *oauth2.Config, httpClient *http.Client, client *resolvedOAuthClient, deviceCode, resource string) (*oauth2.Token, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
		"client_id":   {client.ClientID},
		"resource":    {resource},
	}
	if client.ClientSecret != "" {
		form.Set("client_secret", client.ClientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		return nil, &devicePollError{code: failure.Error}
	}
	var success struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(data, &success); err != nil || success.AccessToken == "" {
		return nil, fmt.Errorf("device token response is invalid")
	}
	token := &oauth2.Token{AccessToken: success.AccessToken, TokenType: success.TokenType, RefreshToken: success.RefreshToken}
	if success.ExpiresIn > 0 {
		token.Expiry = time.Now().Add(time.Duration(success.ExpiresIn) * time.Second)
	}
	if strings.TrimSpace(success.Scope) != "" {
		// Surface the granted scope through extras so persistence treats
		// the token response as authoritative.
		token = token.WithExtra(map[string]any{"scope": success.Scope})
	}
	return token, nil
}

type devicePollError struct{ code string }

func (e *devicePollError) Error() string { return "device authorization " + e.code }

func deviceErrorCode(err error) string {
	var pollErr *devicePollError
	if errors.As(err, &pollErr) {
		return pollErr.code
	}
	return ""
}

func isDevicePending(err error) bool { return deviceErrorCode(err) == "authorization_pending" }

func isDeviceSlowDown(err error) bool { return deviceErrorCode(err) == "slow_down" }

// grantedScopes resolves the persisted scope set for a fresh grant. The
// token response is authoritative: a returned scope string wins. Otherwise
// the requested scopes stand (an omitted scope is omitted on the wire and
// the server grants its default set, which the response then describes).
func grantedScopes(token *oauth2.Token, requested []string) []string {
	if raw, ok := token.Extra("scope").(string); ok && strings.TrimSpace(raw) != "" {
		return strings.Fields(raw)
	}
	return append([]string(nil), requested...)
}

func tokenSetFromOAuth(token *oauth2.Token, issuer, clientID string, scopes []string) *TokenSet {
	out := &TokenSet{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, Expiry: token.Expiry, Issuer: issuer, ClientID: clientID, Scopes: grantedScopes(token, scopes)}
	if token.TokenType != "" {
		out.TokenType = token.TokenType
	} else {
		out.TokenType = "Bearer"
	}
	return out
}

// refreshOAuthToken exchanges a refresh token for a new credential under a
// cross-process fence held across read→POST→persist, so two processes cannot
// double-spend a single-use refresh token. When a peer already rotated (the
// stored grant differs from expectedRefresh and is live), the winner's pair
// is adopted without POSTing the burned grant. Responses that omit
// refresh_token or scope (RFC 6749 §6) carry the prior values forward.
// The resource indicator is sent when resource is non-empty.
// The scopes argument is the first-grant selection (see
// selectEffectiveScopes); an empty selection falls back to the stored grant
// below and is omitted on the wire.
func refreshOAuthToken(ctx context.Context, store *tokenStore, meta *serverMetadata, client *resolvedOAuthClient, scopes []string, resource string, expectedRefresh string, httpClient *http.Client) (*TokenSet, error) {
	unlock, err := store.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, ok, err := readTokenFile[TokenSet](store.path(".json"))
	if err != nil {
		return nil, err
	}
	if !ok || current.RefreshToken == "" || !issuerMatches(current.Issuer, meta.Issuer) {
		return nil, fmt.Errorf("no refresh token available")
	}
	if expectedRefresh != "" && current.RefreshToken != expectedRefresh {
		if current.Expiry.IsZero() || time.Now().Add(time.Minute).Before(current.Expiry) {
			return &current, nil
		}
	}
	effectiveScopes := scopes
	if len(effectiveScopes) == 0 {
		effectiveScopes = current.Scopes
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {current.RefreshToken},
		"client_id":     {client.ClientID},
	}
	if resource != "" {
		form.Set("resource", resource)
	}
	if len(effectiveScopes) > 0 {
		form.Set("scope", strings.Join(effectiveScopes, " "))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if client.ClientSecret != "" {
		if client.AuthStyle == oauth2.AuthStyleInHeader {
			request.SetBasicAuth(url.QueryEscape(client.ClientID), url.QueryEscape(client.ClientSecret))
		} else {
			form.Set("client_secret", client.ClientSecret)
			request.Body = io.NopCloser(strings.NewReader(form.Encode()))
			request.ContentLength = int64(len(form.Encode()))
		}
	}
	httpClient = oauthHTTPClient(httpClient)
	response, err := httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("refresh OAuth token: request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("refresh OAuth token: unreadable response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &refreshError{status: response.StatusCode, body: data}
	}
	var success struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(data, &success); err != nil || success.AccessToken == "" {
		return nil, fmt.Errorf("refresh OAuth token: response is invalid")
	}
	refreshed := &TokenSet{AccessToken: success.AccessToken, Issuer: meta.Issuer, ClientID: client.ClientID}
	if success.TokenType != "" {
		refreshed.TokenType = success.TokenType
	} else {
		refreshed.TokenType = "Bearer"
	}
	if success.ExpiresIn > 0 {
		refreshed.Expiry = time.Now().Add(time.Duration(success.ExpiresIn) * time.Second)
	}
	if success.RefreshToken != "" {
		refreshed.RefreshToken = success.RefreshToken
	} else {
		refreshed.RefreshToken = current.RefreshToken
	}
	if success.Scope != "" {
		refreshed.Scopes = strings.Fields(success.Scope)
	} else {
		refreshed.Scopes = append([]string(nil), effectiveScopes...)
	}
	latest, ok, err := readTokenFile[TokenSet](store.path(".json"))
	if err != nil {
		return nil, err
	}
	if ok && latest.RefreshToken == current.RefreshToken {
		if err := writeTokenFile(store.path(".json"), *refreshed); err != nil {
			return nil, err
		}
	} else if ok {
		return &latest, nil
	}
	return refreshed, nil
}

// refreshError carries a non-2xx token-endpoint response for invalid_client
// detection without logging secrets or payloads.
type refreshError struct {
	status int
	body   []byte
}

func (e *refreshError) Error() string {
	var failure struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(e.body, &failure) == nil && failure.Error != "" {
		return "refresh OAuth token: " + failure.Error
	}
	return "refresh OAuth token: unexpected status"
}
