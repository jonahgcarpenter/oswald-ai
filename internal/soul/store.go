// Package soul loads read-only operator-managed profile system prompts.
package soul

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const maxSoulBytes = 1 << 20

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Store reads one existing profile and never creates, migrates, or deletes souls.
type Store struct{ path, profileRoot, profile string }

// NewProfileStore uses the default soul only when the profile soul is absent.
func NewProfileStore(root, profile, defaultSoul string) *Store {
	return &Store{path: defaultSoul, profileRoot: root, profile: profile}
}

// Read loads current operator policy, rejecting unsafe paths and other owners.
func (s *Store) Read(ctx context.Context, owner string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil || s.profileRoot == "" || owner != s.profile || !safeID.MatchString(owner) {
		return "", errors.New("soul profile mismatch")
	}
	if err := validateProfileRoot(s.profileRoot); err != nil {
		return "", err
	}
	content, err := readSoul(filepath.Join(s.profileRoot, "SOUL.md"), true)
	if errors.Is(err, os.ErrNotExist) {
		return readSoul(s.path, false)
	}
	return content, err
}

func validateProfileRoot(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	path := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, path), path) {
		if part == "" {
			continue
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (path == abs && info.Mode().Perm()&0077 != 0) {
			return errors.New("unsafe profile soul directory")
		}
	}
	return nil
}

func readSoul(path string, private bool) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || (private && info.Mode().Perm()&0077 != 0) {
		return "", errors.New("unsafe soul file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSoulBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxSoulBytes {
		return "", errors.New("soul file exceeds size limit")
	}
	return string(data), nil
}
