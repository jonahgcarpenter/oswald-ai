// Package chatgpt owns the ChatGPT subscription transport: a profile-scoped
// OAuth token store plus a Chat client that authenticates with the user's own
// subscription instead of an API key. Tokens are operator state under
// <profileRoot>/providers/<name>/ and are never logged.
package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// ClientID is the public Codex OAuth client. No secret is embedded;
	// PKCE protects the exchange.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// AuthBaseURL is the token issuer. Overridden in tests.
	AuthBaseURL = "https://auth.openai.com"
	// DefaultAPI is the subscription backend base URL.
	DefaultAPI = "https://chatgpt.com/backend-api/codex"
)

var (
	authBaseURL    = AuthBaseURL
	nameRE         = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	maxFileBytes   = 64 * 1024
	refreshSkew    = 60 * time.Second
	refreshMu      sync.Mutex
	refreshPromise map[string]*refreshCall
)

type refreshCall struct {
	done chan struct{}
	err  error
}

func init() { refreshPromise = make(map[string]*refreshCall) }

// TokenSet is the persisted subscription credential. Values must never be logged.
type TokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

// validateName fails closed on traversal and reserved names.
func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || !nameRE.MatchString(name) {
		return errors.New("invalid provider name")
	}
	return nil
}

func dirForRoot(root, name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, "providers", name), nil
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("provider token directory is not a safe directory")
	}
	if info.Mode().Perm()&0077 == 0 {
		return nil
	}
	return os.Chmod(dir, 0700)
}

// Store is the profile-scoped token store for one ChatGPT provider.
type Store struct {
	mu   sync.Mutex
	dir  string
	name string
}

// NewStore binds a store to one profile root and provider name. It performs no I/O.
func NewStore(profileRoot, provider string) (*Store, error) {
	dir, err := dirForRoot(profileRoot, provider)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, name: provider}, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "auth.json") }

func (s *Store) pendingPath() string { return filepath.Join(s.dir, "pending.json") }

func (s *Store) lockPath() string { return filepath.Join(s.dir, "auth.lock") }

func (s *Store) locked(ctx context.Context, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensurePrivateDir(s.dir); err != nil {
		return err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("provider lock file is not private")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("provider refresh fence timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// Load returns the stored credential, or false when never authorized.
func (s *Store) Load(ctx context.Context) (TokenSet, bool, error) {
	var out TokenSet
	var ok bool
	if err := s.locked(ctx, func() error {
		tokens, found, err := readFile(s.path())
		if err != nil || !found {
			return err
		}
		out, ok = tokens, true
		return nil
	}); err != nil {
		return TokenSet{}, false, err
	}
	if !ok || strings.TrimSpace(out.AccessToken) == "" {
		return TokenSet{}, false, nil
	}
	return out, true, nil
}

// Save persists a credential received from the token endpoint.
func (s *Store) Save(ctx context.Context, tokens TokenSet) error {
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return errors.New("provider credential is incomplete")
	}
	return s.locked(ctx, func() error { return writeFile(s.path(), tokens) })
}

// Clear discards the stored credential so the next use re-authenticates.
func (s *Store) Clear(ctx context.Context) error {
	return s.locked(ctx, func() error {
		if err := os.Remove(s.path()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// PendingAuth is the in-flight browser handoff state: the model starts it,
// the user completes the browser step, and the model finishes the exchange.
// It carries no secret, only the PKCE verifier needed to redeem the code.
type PendingAuth struct {
	Verifier    string `json:"verifier"`
	State       string `json:"state"`
	RedirectURI string `json:"redirect_uri"`
	ExpiresAt   int64  `json:"expires_at"`
}

// SavePending records a started handoff, replacing any previous one.
func (s *Store) SavePending(ctx context.Context, pending PendingAuth) error {
	if strings.TrimSpace(pending.Verifier) == "" || strings.TrimSpace(pending.State) == "" {
		return errors.New("pending authorization is incomplete")
	}
	return s.locked(ctx, func() error { return writePending(s.pendingPath(), pending) })
}

// LoadPending returns the in-flight handoff, or false when none was started.
func (s *Store) LoadPending(ctx context.Context) (PendingAuth, bool, error) {
	var out PendingAuth
	var ok bool
	if err := s.locked(ctx, func() error {
		pending, found, err := readPending(s.pendingPath())
		if err != nil || !found {
			return err
		}
		out, ok = pending, true
		return nil
	}); err != nil {
		return PendingAuth{}, false, err
	}
	if !ok {
		return PendingAuth{}, false, nil
	}
	return out, true, nil
}

// ClearPending discards the in-flight handoff.
func (s *Store) ClearPending(ctx context.Context) error {
	return s.locked(ctx, func() error {
		if err := os.Remove(s.pendingPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// PendingExpired reports whether a handoff is past its deadline.
func PendingExpired(pending PendingAuth, now time.Time) bool {
	if pending.ExpiresAt <= 0 {
		return true
	}
	return !time.Unix(pending.ExpiresAt, 0).After(now)
}

func readPending(path string) (PendingAuth, bool, error) {
	var zero PendingAuth
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, false, nil
		}
		return zero, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return zero, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return zero, false, fmt.Errorf("provider file %s is not private", filepath.Base(path))
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxFileBytes)+1))
	if err != nil {
		return zero, false, err
	}
	if len(data) > maxFileBytes {
		return zero, false, fmt.Errorf("provider file %s exceeds size limit", filepath.Base(path))
	}
	var value PendingAuth
	if err := json.Unmarshal(data, &value); err != nil {
		return zero, false, fmt.Errorf("provider file %s is invalid", filepath.Base(path))
	}
	return value, true, nil
}

func writePending(path string, value PendingAuth) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readFile(path string) (TokenSet, bool, error) {
	var zero TokenSet
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return zero, false, nil
		}
		return zero, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return zero, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return zero, false, fmt.Errorf("provider file %s is not private", filepath.Base(path))
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxFileBytes)+1))
	if err != nil {
		return zero, false, err
	}
	if len(data) > maxFileBytes {
		return zero, false, fmt.Errorf("provider file %s exceeds size limit", filepath.Base(path))
	}
	var value TokenSet
	if err := json.Unmarshal(data, &value); err != nil {
		return zero, false, fmt.Errorf("provider file %s is invalid", filepath.Base(path))
	}
	return value, true, nil
}

func writeFile(path string, value TokenSet) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// RepairProfile tightens permissions on existing provider material without
// creating anything. Missing paths stay missing; symlinks fail closed.
func RepairProfile(profileRoot string) {
	root, err := filepath.Abs(profileRoot)
	if err != nil {
		return
	}
	base := filepath.Join(root, "providers")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if validateName(entry.Name()) != nil {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		} else if info.Mode().Perm()&0077 != 0 {
			_ = os.Chmod(dir, 0700)
		}
		for _, name := range []string{"auth.json", "auth.lock", "pending.json"} {
			p := filepath.Join(dir, name)
			if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				continue
			} else if info.Mode().Perm()&0077 != 0 {
				_ = os.Chmod(p, 0600)
			}
		}
	}
}

// Expired reports whether the token should be refreshed (60s skew).
func Expired(tokens TokenSet, now time.Time) bool {
	if tokens.ExpiresAt <= 0 {
		return false
	}
	return !time.Unix(tokens.ExpiresAt, 0).After(now.Add(refreshSkew))
}

// GeneratePKCE creates a verifier/challenge pair (S256).
func GeneratePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// GenerateState creates a CSRF state value.
func GenerateState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// BuildAuthorizeURL builds the browser authorization URL.
func BuildAuthorizeURL(redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", "oswald")
	return strings.TrimRight(authBaseURL, "/") + "/oauth/authorize?" + q.Encode()
}

// AccountIDFromIDToken extracts the ChatGPT account id from a JWT without
// verifying the signature. It returns "" when absent.
func AccountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	var id string
	if raw, ok := claims["chatgpt_account_id"]; ok {
		_ = json.Unmarshal(raw, &id)
		if strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	if raw, ok := claims["https://api.openai.com/auth.chatgpt_account_id"]; ok {
		_ = json.Unmarshal(raw, &id)
		if strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	if raw, ok := claims["organizations"]; ok {
		var orgs []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &orgs); err == nil && len(orgs) > 0 {
			_ = json.Unmarshal(orgs[0]["id"], &id)
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// TokenResponse is the token endpoint response. Fields are never logged.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
}

// ExchangeCode trades an authorization code for tokens.
func ExchangeCode(ctx context.Context, httpClient *http.Client, code, redirectURI, verifier string) (TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", ClientID)
	form.Set("code_verifier", verifier)
	return postToken(ctx, httpClient, form)
}

// RefreshTokens trades a refresh token for a new pair. Single-use rotation:
// callers persist the result before using it.
func RefreshTokens(ctx context.Context, httpClient *http.Client, refreshToken string) (TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", ClientID)
	form.Set("refresh_token", refreshToken)
	return postTokenJSON(ctx, httpClient, form)
}

func httpClientOrDefault(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func postToken(ctx context.Context, httpClient *http.Client, form url.Values) (TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(authBaseURL, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClientOrDefault(httpClient).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return TokenResponse{}, ctx.Err()
		}
		return TokenResponse{}, errors.New("token exchange failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, errors.New("token exchange failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			return TokenResponse{}, errors.New("refresh token rejected")
		}
		return TokenResponse{}, errors.New("token exchange failed")
	}
	var out TokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return TokenResponse{}, errors.New("token exchange failed")
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return TokenResponse{}, errors.New("token exchange failed")
	}
	return out, nil
}

func postTokenJSON(ctx context.Context, httpClient *http.Client, form url.Values) (TokenResponse, error) {
	payload := map[string]string{}
	for k := range form {
		payload[k] = form.Get(k)
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(authBaseURL, "/")+"/oauth/token", strings.NewReader(string(data)))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClientOrDefault(httpClient).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return TokenResponse{}, ctx.Err()
		}
		return TokenResponse{}, errors.New("token refresh failed")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, errors.New("token refresh failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			return TokenResponse{}, errors.New("refresh token rejected")
		}
		return TokenResponse{}, errors.New("token refresh failed")
	}
	var out TokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return TokenResponse{}, errors.New("token refresh failed")
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return TokenResponse{}, errors.New("token refresh failed")
	}
	return out, nil
}
