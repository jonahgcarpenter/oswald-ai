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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, false, fmt.Errorf("bind OAuth callback: %w", err)
	}
	defer listener.Close()
	redirectURI := "http://" + listener.Addr().String() + "/callback"
	client, err := resolveFlowClient(ctx, store, oauth, meta, redirectURI, forceDCR)
	if err != nil {
		return nil, false, err
	}
	verifier := oauth2.GenerateVerifier()
	state := rand.Text()
	authURL := oauth2Config(client, meta, oauth.Scopes).AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", oauthResource(meta, mcpURL)),
	)
	if !opts.NoBrowser {
		_ = openBrowser(authURL)
	}
	fmt.Fprintf(opts.Stdout, "Open this URL to authorize:\n%s\n", authURL)
	if opts.NoBrowser {
		fmt.Fprintln(opts.Stdout, "After approving, paste the full redirect URL or the code here:")
	}
	codeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, callbackErr := waitForCode(codeCtx, listener, state, opts)
	if callbackErr != nil {
		return nil, false, callbackErr
	}
	tokens, err := exchangeCode(ctx, client, meta, oauth.Scopes, oauthResource(meta, mcpURL), code, verifier, opts.HTTP)
	if err != nil {
		if client.Method == clientMethodCIMD && isInvalidClient(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return tokens, false, nil
}

func resolveFlowClient(ctx context.Context, store *tokenStore, oauth *OAuthConfig, meta *serverMetadata, redirectURI string, forceDCR bool) (*resolvedOAuthClient, error) {
	if !forceDCR {
		if client, err := resolveOAuthClient(ctx, oauth, store, meta, redirectURI); err == nil {
			return client, nil
		} else if err != errNoCIMD {
			return nil, err
		}
	}
	return performDCR(ctx, store, meta, redirectURI, oauth.Scopes)
}

func waitForCode(ctx context.Context, listener net.Listener, state string, opts FlowOptions) (string, error) {
	type result struct {
		code string
		err  error
	}
	calls := make(chan result, 1)
	go func() {
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			query := r.URL.Query()
			if errText := query.Get("error"); errText != "" {
				calls <- result{err: fmt.Errorf("authorization server refused: %s", errText)}
				_, _ = io.WriteString(w, "Authorization failed. You can close this tab.")
				return
			}
			if query.Get("state") != state {
				calls <- result{err: fmt.Errorf("authorization state mismatch")}
				_, _ = io.WriteString(w, "Authorization failed state check. You can close this tab.")
				return
			}
			if query.Get("code") == "" {
				calls <- result{err: fmt.Errorf("authorization response has no code")}
				_, _ = io.WriteString(w, "Authorization failed. You can close this tab.")
				return
			}
			calls <- result{code: query.Get("code")}
			_, _ = io.WriteString(w, "Authorized. You can close this tab.")
		})}
		_ = server.Serve(listener)
	}()
	if opts.NoBrowser && opts.Stdin != nil {
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

func parsePastedCode(line, state string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("no authorization response pasted")
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
	if httpClient == nil {
		httpClient = discoveryClient()
	}
	tokenCtx := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
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
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = discoveryClient()
	}
	// Device flow needs a client but no redirect URI; resolve with a placeholder
	// loopback URI, then register dynamically when required.
	client, err := resolveOAuthClient(ctx, oauth, store, meta, "http://127.0.0.1:1/callback")
	if err != nil {
		if err != errNoCIMD {
			return nil, err
		}
		if client, err = performDCR(ctx, store, meta, "http://127.0.0.1:1/callback", oauth.Scopes); err != nil {
			return nil, err
		}
	}
	form := url.Values{"client_id": {client.ClientID}, "scope": {strings.Join(oauth.Scopes, " ")}}
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
	cfg := oauth2Config(client, meta, oauth.Scopes)
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
		return tokenSetFromOAuth(token, meta.Issuer, client.ClientID, oauth.Scopes), nil
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

func tokenSetFromOAuth(token *oauth2.Token, issuer, clientID string, scopes []string) *TokenSet {
	out := &TokenSet{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, Expiry: token.Expiry, Issuer: issuer, ClientID: clientID, Scopes: append([]string(nil), scopes...)}
	if token.TokenType != "" {
		out.TokenType = token.TokenType
	} else {
		out.TokenType = "Bearer"
	}
	return out
}

// refreshOAuthToken exchanges a refresh token for a new credential and
// persists it when the store holds the same refresh token (no clobbering a
// newer parallel refresh).
func refreshOAuthToken(ctx context.Context, store *tokenStore, meta *serverMetadata, client *resolvedOAuthClient, scopes []string, httpClient *http.Client) (*TokenSet, error) {
	current, ok, err := store.LoadTokens(ctx)
	if err != nil {
		return nil, err
	}
	if !ok || current.RefreshToken == "" || current.Issuer != meta.Issuer {
		return nil, fmt.Errorf("no refresh token available")
	}
	if httpClient == nil {
		httpClient = discoveryClient()
	}
	cfg := oauth2Config(client, meta, scopes)
	tokenCtx := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	source := cfg.TokenSource(tokenCtx, &oauth2.Token{RefreshToken: current.RefreshToken, Expiry: time.Now().Add(-time.Minute)})
	token, err := source.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh OAuth token: %w", err)
	}
	refreshed := tokenSetFromOAuth(token, meta.Issuer, client.ClientID, scopes)
	if token.RefreshToken == "" {
		refreshed.RefreshToken = current.RefreshToken
	}
	latest, ok, err := store.LoadTokens(ctx)
	if err != nil {
		return nil, err
	}
	if ok && latest.RefreshToken == current.RefreshToken {
		if err := store.SaveTokens(ctx, *refreshed); err != nil {
			return nil, err
		}
	}
	return refreshed, nil
}
