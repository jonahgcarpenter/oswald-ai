package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// tokenDirName is the profile-local directory holding OAuth material. It
// lives beside config.yaml, souls, and state.db inside the private profile
// root; directory backups already cover it.
const tokenDirName = "mcp-tokens"

const maxTokenFileBytes = 64 * 1024

// TokenSet is the persisted credential for one OAuth MCP server. Tokens are
// bound to the discovery issuer and must only be sent to that issuer's
// endpoints. Values must never be logged.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expires_at,omitempty"`
	Scopes       []string  `json:"scopes,omitempty"`
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
}

// ClientRecord is a dynamically registered (or CIMD loopback) client
// credential persisted across CLI runs so the service can refresh without
// re-registering.
type ClientRecord struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	Method       string   `json:"method"`
}

// Client registration methods recorded in ClientRecord.Method.
const (
	clientMethodOperator = "operator"
	clientMethodCIMD     = "cimd"
	clientMethodDCR      = "dcr"
)

// tokenStore owns the private per-profile OAuth files for one MCP server:
// <server>.json (tokens), <server>.client.json (DCR credentials),
// <server>.meta.json (authorization-server metadata cache), and
// <server>.cimd-off (marker: CIMD refused, use DCR).
type tokenStore struct {
	dir    string
	server string
}

func newTokenStore(profileRoot, server string) (*tokenStore, error) {
	if err := validateServerName(server); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(profileRoot)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(filepath.Join(root, tokenDirName)); err != nil {
		return nil, err
	}
	return &tokenStore{dir: filepath.Join(root, tokenDirName), server: server}, nil
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
		return errors.New("OAuth token directory is not a safe directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	return nil
}

func (s *tokenStore) path(suffix string) string {
	return filepath.Join(s.dir, s.server+suffix)
}

// locked runs fn while holding the server's exclusive lock file. Token,
// client, metadata, and marker files are always read and written under it.
func (s *tokenStore) locked(ctx context.Context, fn func() error) error {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// acquire holds the server's exclusive lock file until the returned release
// func runs. It backs the cross-process refresh fence: callers must hold it
// across read→POST→persist so two processes cannot double-spend a
// single-use refresh token.
func (s *tokenStore) acquire(ctx context.Context) (func(), error) {
	lock, err := os.OpenFile(s.path(".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil {
		lock.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, errors.New("OAuth lock file is not private")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			lock.Close()
			return nil, err
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			lock.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			lock.Close()
			return nil, fmt.Errorf("OAuth refresh fence timeout")
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}, nil
}

func readTokenFile[T any](path string) (T, bool, error) {
	var zero T
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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
		return zero, false, fmt.Errorf("OAuth file %s is not private", filepath.Base(path))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes+1))
	if err != nil {
		return zero, false, err
	}
	if int64(len(data)) > maxTokenFileBytes {
		return zero, false, fmt.Errorf("OAuth file %s exceeds size limit", filepath.Base(path))
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return zero, false, fmt.Errorf("OAuth file %s is invalid", filepath.Base(path))
	}
	return value, true, nil
}

func writeTokenFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}

// LoadTokens returns the stored credential, or false when the server has
// never completed OAuth.
func (s *tokenStore) LoadTokens(ctx context.Context) (TokenSet, bool, error) {
	var tokens TokenSet
	var ok bool
	var loadErr error
	if err := s.locked(ctx, func() error {
		tokens, ok, loadErr = readTokenFile[TokenSet](s.path(".json"))
		return loadErr
	}); err != nil {
		return TokenSet{}, false, err
	}
	if !ok || tokens.AccessToken == "" || tokens.Issuer == "" {
		return TokenSet{}, false, nil
	}
	return tokens, true, nil
}

// SaveTokens persists a credential received from the token endpoint.
func (s *tokenStore) SaveTokens(ctx context.Context, tokens TokenSet) error {
	if tokens.AccessToken == "" || tokens.Issuer == "" {
		return errors.New("OAuth credential is incomplete")
	}
	return s.locked(ctx, func() error { return writeTokenFile(s.path(".json"), tokens) })
}

// LoadClient returns a previously registered dynamic client, if any.
func (s *tokenStore) LoadClient(ctx context.Context) (ClientRecord, bool, error) {
	var record ClientRecord
	var ok bool
	var loadErr error
	if err := s.locked(ctx, func() error {
		record, ok, loadErr = readTokenFile[ClientRecord](s.path(".client.json"))
		return loadErr
	}); err != nil {
		return ClientRecord{}, false, err
	}
	if !ok || record.ClientID == "" {
		return ClientRecord{}, false, nil
	}
	return record, true, nil
}

// SaveClient persists a dynamically registered client.
func (s *tokenStore) SaveClient(ctx context.Context, record ClientRecord) error {
	if record.ClientID == "" {
		return errors.New("OAuth client record is incomplete")
	}
	return s.locked(ctx, func() error { return writeTokenFile(s.path(".client.json"), record) })
}

// CIMDDisabled reports whether a previous run recorded CIMD refusal.
func (s *tokenStore) CIMDDisabled(ctx context.Context) (bool, error) {
	var disabled bool
	if err := s.locked(ctx, func() error {
		_, ok, err := readTokenFile[json.RawMessage](s.path(".cimd-off"))
		disabled = ok
		return err
	}); err != nil {
		return false, err
	}
	return disabled, nil
}

// DisableCIMD records that the server refused CIMD so later runs use DCR.
func (s *tokenStore) DisableCIMD(ctx context.Context) error {
	return s.locked(ctx, func() error { return writeTokenFile(s.path(".cimd-off"), struct{}{}) })
}

// LoadMetadata returns the cached authorization-server metadata, if any.
func (s *tokenStore) LoadMetadata(ctx context.Context) (serverMetadata, bool, error) {
	var meta serverMetadata
	var ok bool
	var loadErr error
	if err := s.locked(ctx, func() error {
		meta, ok, loadErr = readTokenFile[serverMetadata](s.path(".meta.json"))
		return loadErr
	}); err != nil {
		return serverMetadata{}, false, err
	}
	if !ok || meta.Issuer == "" || meta.TokenEndpoint == "" {
		return serverMetadata{}, false, nil
	}
	return meta, true, nil
}

// SaveMetadata caches authorization-server metadata.
func (s *tokenStore) SaveMetadata(ctx context.Context, meta serverMetadata) error {
	if meta.Issuer == "" || meta.TokenEndpoint == "" {
		return errors.New("OAuth metadata is incomplete")
	}
	return s.locked(ctx, func() error { return writeTokenFile(s.path(".meta.json"), meta) })
}

// ClearTokens discards the stored credential, used when the configured
// client identity changes or a grant is known dead.
func (s *tokenStore) ClearTokens(ctx context.Context) error {
	return s.locked(ctx, func() error {
		if err := os.Remove(s.path(".json")); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// ClearClient discards a dynamically registered client alongside stale
// metadata so the next flow re-registers instead of retrying a dead ID.
func (s *tokenStore) ClearClient(ctx context.Context) error {
	return s.locked(ctx, func() error {
		for _, suffix := range []string{".client.json", ".meta.json"} {
			if err := os.Remove(s.path(suffix)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
}

// BindIssuerIfEmpty adopts the discovery issuer once for credentials
// persisted before issuer binding. It binds only files that already hold an
// access token with an empty issuer; otherwise it reports false.
func (s *tokenStore) BindIssuerIfEmpty(ctx context.Context, issuer string) (TokenSet, bool, error) {
	if strings.TrimSpace(issuer) == "" {
		return TokenSet{}, false, nil
	}
	var adopted TokenSet
	var ok bool
	if err := s.locked(ctx, func() error {
		raw, found, err := readTokenFile[TokenSet](s.path(".json"))
		if err != nil || !found || raw.AccessToken == "" || raw.Issuer != "" {
			ok = false
			return err
		}
		raw.Issuer = issuer
		if err := writeTokenFile(s.path(".json"), raw); err != nil {
			return err
		}
		adopted, ok = raw, true
		return nil
	}); err != nil {
		return TokenSet{}, false, err
	}
	return adopted, ok, nil
}

// CachedRedirectURI returns the first redirect URI of a usable cached
// dynamic client so re-login can reuse it instead of leaking a new
// registration per ephemeral port.
func (s *tokenStore) CachedRedirectURI(ctx context.Context) (string, bool, error) {
	record, ok, err := s.LoadClient(ctx)
	if err != nil || !ok || len(record.RedirectURIs) == 0 {
		return "", false, err
	}
	if strings.TrimSpace(record.RedirectURIs[0]) == "" {
		return "", false, nil
	}
	return record.RedirectURIs[0], true, nil
}
