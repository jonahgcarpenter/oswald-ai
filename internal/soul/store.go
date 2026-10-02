// Package soul loads private operator-managed system prompts.
package soul

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const maxSoulBytes = 1 << 20

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Store owns the global template and per-user system prompts.
type Store struct {
	path        string
	profileRoot string
	profile     string
}

// NewStore uses path as the default template and its directory as the user root.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// NewProfileStore reads an operator-owned profile soul, falling back to the
// default soul without copying or creating operator files.
func NewProfileStore(root, profile, defaultSoul string) *Store {
	return &Store{path: defaultSoul, profileRoot: root, profile: profile}
}

// MigrateTemplate preserves an operator-managed template at the old path.
// If both paths exist, differing contents require operator reconciliation.
func (s *Store) MigrateTemplate(legacy string) error {
	old, err := readSoul(legacy, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy soul template: %w", err)
	}
	current, err := readSoul(s.path, false)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(legacy, s.path); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(s.path)); err != nil {
			return err
		}
		return syncDir(filepath.Dir(legacy))
	}
	if err != nil {
		return err
	}
	if old != current {
		return errors.New("old and new soul templates differ; reconcile before startup")
	}
	if err := os.Remove(legacy); err != nil {
		return err
	}
	return syncDir(filepath.Dir(legacy))
}

// Read returns a user's operator-owned SOUL.md, creating it from the current
// template only if missing. Concurrent first reads never observe a partial copy.
func (s *Store) Read(ctx context.Context, userID string) (string, error) {
	if s != nil && s.profileRoot != "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if userID != s.profile || !safeID.MatchString(userID) {
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
	dir, err := s.userDir(ctx, userID, true)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "SOUL.md")
	content, err := readSoul(path, true)
	if err == nil {
		return content, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	template, err := readSoul(s.path, false)
	if err != nil {
		return "", fmt.Errorf("read soul template: %w", err)
	}
	file, err := os.CreateTemp(dir, ".soul-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return "", err
	}
	if _, err := file.WriteString(template); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// A hard link publishes the fully synced copy only if SOUL.md is absent.
	if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return readSoul(path, true)
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

// Delete removes only the user's private SOUL.md; it does not remove the
// directory or the global template.
func (s *Store) Delete(ctx context.Context, userID string) error {
	dir, err := s.userDir(ctx, userID, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "SOUL.md")
	if _, err := readSoul(path, true); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) userDir(ctx context.Context, userID string, create bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s == nil || s.path == "" || !safeID.MatchString(userID) {
		return "", errors.New("invalid soul store or user ID")
	}
	root, err := filepath.Abs(filepath.Dir(s.path))
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, userID)
	path := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(dir, path), path) {
		if part == "" {
			continue
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(path)
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || (path == dir && info.Mode().Perm()&0077 != 0) {
			return "", errors.New("unsafe soul directory")
		}
	}
	return dir, nil
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
