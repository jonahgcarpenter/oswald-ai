package chatgpt

import (
	"context"
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
)

// AuthorizeOptions is the interactive login input. Stdout receives the URL
// and device instructions; no secret is ever printed.
type AuthorizeOptions struct {
	ProfileRoot string
	Provider    string
	NoBrowser   bool
	Timeout     time.Duration
	Stdin       io.Reader
	Stdout      io.Writer
	HTTPClient  *http.Client
}

var openBrowser = func(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Start()
}

// Start begins a model-led handoff: it generates the PKCE pair, persists the
// pending state, and returns the URL the model relays to the user. No secret
// is returned; the exchange happens in Finish after the browser step.
func Start(ctx context.Context, profileRoot, provider string, timeout time.Duration) (authURL string, expiresAt time.Time, err error) {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(profileRoot) == "" {
		return "", time.Time{}, errors.New("provider and profile root are required")
	}
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	store, err := NewStore(profileRoot, provider)
	if err != nil {
		return "", time.Time{}, err
	}
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return "", time.Time{}, err
	}
	state, err := GenerateState()
	if err != nil {
		return "", time.Time{}, err
	}
	redirectURI := "http://localhost:1455/auth/callback"
	expiresAt = time.Now().Add(timeout)
	if err := store.SavePending(ctx, PendingAuth{Verifier: verifier, State: state, RedirectURI: redirectURI, ExpiresAt: expiresAt.Unix()}); err != nil {
		return "", time.Time{}, err
	}
	return BuildAuthorizeURL(redirectURI, state, challenge), expiresAt, nil
}

// ExtractCode accepts a bare authorization code or a full redirect URL the
// user pasted back, returning the code. A redirect URL with a mismatched
// state fails closed; error responses fail closed without details.
func ExtractCode(input, wantState string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", errors.New("authorization response is empty")
	}
	if strings.Contains(trimmed, "://") || strings.HasPrefix(trimmed, "localhost") || strings.Contains(trimmed, "code=") {
		raw := trimmed
		if !strings.Contains(raw, "://") {
			raw = "http://" + strings.TrimLeft(raw, "/")
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", errors.New("authorization response is not a valid code or URL")
		}
		q := parsed.Query()
		if q.Get("error") != "" {
			return "", errors.New("authorization was denied in the browser")
		}
		if state := strings.TrimSpace(q.Get("state")); state != "" && state != wantState {
			return "", errors.New("authorization callback state mismatch")
		}
		if code := strings.TrimSpace(q.Get("code")); code != "" {
			return code, nil
		}
		return "", errors.New("authorization response did not include a code")
	}
	if strings.ContainsAny(trimmed, " \t\r\n") || len(trimmed) > 4096 {
		return "", errors.New("authorization response is not a valid code or URL")
	}
	return trimmed, nil
}

// Finish redeems a pasted code/URL against the pending handoff, persists the
// tokens, and clears pending. It fails closed when no handoff was started or
// the deadline passed.
func Finish(ctx context.Context, profileRoot, provider, codeOrURL string, httpClient *http.Client) error {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(profileRoot) == "" {
		return errors.New("provider and profile root are required")
	}
	store, err := NewStore(profileRoot, provider)
	if err != nil {
		return err
	}
	pending, ok, err := store.LoadPending(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no authorization was started for this provider")
	}
	if PendingExpired(pending, time.Now()) {
		_ = store.ClearPending(ctx)
		return errors.New("authorization handoff expired; start again")
	}
	code, err := ExtractCode(codeOrURL, pending.State)
	if err != nil {
		return err
	}
	resp, err := ExchangeCode(ctx, httpClient, code, pending.RedirectURI, pending.Verifier)
	if err != nil {
		return err
	}
	tokens := TokenSet{AccessToken: resp.AccessToken, RefreshToken: resp.RefreshToken, AccountID: AccountIDFromIDToken(resp.IDToken)}
	if resp.ExpiresIn > 0 {
		tokens.ExpiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Unix()
	}
	if err := store.Save(ctx, tokens); err != nil {
		return err
	}
	_ = store.ClearPending(ctx)
	return nil
}

// Authorize runs the browser PKCE flow (or device flow on listener failure)
// and persists the resulting tokens.
func Authorize(ctx context.Context, opts AuthorizeOptions) error {
	provider := strings.TrimSpace(opts.Provider)
	if provider == "" || opts.ProfileRoot == "" {
		return errors.New("provider and profile root are required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	store, err := NewStore(opts.ProfileRoot, provider)
	if err != nil {
		return err
	}
	tokens, err := runBrowserFlow(ctx, opts)
	if err != nil {
		return err
	}
	if err := store.Save(ctx, tokens); err != nil {
		return err
	}
	if opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "Authorized %s with ChatGPT subscription\n", provider)
	}
	return nil
}

func runBrowserFlow(ctx context.Context, opts AuthorizeOptions) (TokenSet, error) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return TokenSet{}, err
	}
	state, err := GenerateState()
	if err != nil {
		return TokenSet{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		return runDeviceFlow(ctx, opts)
	}
	defer listener.Close()
	redirectURI := "http://localhost:1455/auth/callback"
	authURL := BuildAuthorizeURL(redirectURI, state, challenge)
	if opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "Open this URL to authorize:\n%s\n", authURL)
	}
	if !opts.NoBrowser {
		openBrowser(authURL)
	}
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := &http.ServeMux{}
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			select {
			case errCh <- errors.New("authorization callback state mismatch"):
			default:
			}
			return
		}
		if q.Get("error") != "" {
			http.Error(w, "authorization failed", http.StatusBadRequest)
			select {
			case errCh <- errors.New("authorization callback returned an error"):
			default:
			}
			return
		}
		code := strings.TrimSpace(q.Get("code"))
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			select {
			case errCh <- errors.New("authorization callback did not include a code"):
			default:
			}
			return
		}
		_, _ = io.WriteString(w, "Authorized. You may close this window.")
		select {
		case codeCh <- code:
		default:
		}
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	var code string
	select {
	case <-ctx.Done():
		return TokenSet{}, ctx.Err()
	case err := <-errCh:
		return TokenSet{}, err
	case code = <-codeCh:
	}
	resp, err := ExchangeCode(ctx, opts.HTTPClient, code, redirectURI, verifier)
	if err != nil {
		return TokenSet{}, err
	}
	out := TokenSet{AccessToken: resp.AccessToken, RefreshToken: resp.RefreshToken, AccountID: AccountIDFromIDToken(resp.IDToken)}
	if resp.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(resp.ExpiresIn) * time.Second).Unix()
	}
	return out, nil
}

func runDeviceFlow(ctx context.Context, opts AuthorizeOptions) (TokenSet, error) {
	client := httpClientOrDefault(opts.HTTPClient)
	form := url.Values{}
	form.Set("client_id", ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(authBaseURL, "/")+"/api/accounts/deviceauth/usercode", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return TokenSet{}, ctx.Err()
		}
		return TokenSet{}, errors.New("device authorization failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenSet{}, errors.New("device authorization failed")
	}
	return TokenSet{}, errors.New("device flow is not enabled for this provider")
}
