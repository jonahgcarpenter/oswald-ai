package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// TokenState reports whether OAuth tokens exist for one profile server and
// whether the stored access token is past its expiry. No secret material is
// returned; callers only learn presence and freshness for status output.
func TokenState(ctx context.Context, profileRoot, server string) (present, expired bool, err error) {
	store, err := newTokenStore(profileRoot, server)
	if err != nil {
		return false, false, err
	}
	set, ok, err := store.LoadTokens(ctx)
	if err != nil {
		return false, false, err
	}
	if !ok {
		return false, false, nil
	}
	if !set.Expiry.IsZero() && time.Now().After(set.Expiry) {
		return true, true, nil
	}
	return true, false, nil
}

// RemoveServerTokens deletes one profile server's private directory
// (mcp/<server>/ with its token files), reporting whether anything was
// stored. The next run re-discovers.
func RemoveServerTokens(profileRoot, server string) (bool, error) {
	if err := validateServerName(server); err != nil {
		return false, err
	}
	root, err := filepath.Abs(profileRoot)
	if err != nil {
		return false, errors.New("invalid configuration root")
	}
	dir := serverDirForRoot(root, server)
	info, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, errors.New("token store is unavailable")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("token store is unavailable")
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, errors.New("token store is unavailable")
	}
	return true, nil
}
