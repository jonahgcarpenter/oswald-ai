// Package files stores bounded private user and memory notes on disk.
package files

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
	"time"
	"unicode/utf8"
)

const separator = "§"

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var errMissingUserDir = errors.New("user directory missing")

// Operation changes one delimiter-separated entry. Replace and remove select
// exactly one entry containing OldText.
type Operation struct {
	Action  string
	Content string
	OldText string
}

// Store owns per-user files below root/<userID>.
type Store struct{ root string }

// NewStore creates a store rooted at the supplied data directory.
func NewStore(root string) *Store { return &Store{root: root} }

// Read returns the private USER.md and MEMORY.md contents; missing files are empty.
func (s *Store) Read(ctx context.Context, userID string) (string, string, error) {
	var user, memory string
	err := s.locked(ctx, userID, false, func(dir string) error {
		var err error
		user, err = readFile(dir, "USER.md", 1375)
		if err != nil {
			return err
		}
		memory, err = readFile(dir, "MEMORY.md", 2200)
		return err
	})
	if errors.Is(err, errMissingUserDir) {
		return "", "", nil
	}
	return user, memory, err
}

// Apply atomically replaces one target file after validating all operations.
// No changes are written if any operation fails.
func (s *Store) Apply(ctx context.Context, userID, target string, ops []Operation) (string, error) {
	name, limit, err := targetFile(target)
	if err != nil {
		return "", err
	}
	if len(ops) == 0 || len(ops) > 20 {
		return "", errors.New("operations must contain 1 to 20 items")
	}
	var result string
	err = s.locked(ctx, userID, true, func(dir string) error {
		current, err := readFile(dir, name, limit)
		if err != nil {
			return err
		}
		entries, err := split(current)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !utf8.ValidString(op.Content) || !utf8.ValidString(op.OldText) {
				return errors.New("invalid UTF-8")
			}
			switch op.Action {
			case "add":
				if op.OldText != "" {
					return errors.New("add cannot specify old_text")
				}
				if err := validEntry(op.Content); err != nil {
					return err
				}
				entries = append(entries, op.Content)
			case "replace", "remove":
				if !utf8.ValidString(op.OldText) || strings.TrimSpace(op.OldText) == "" {
					return errors.New("old_text must be nonempty UTF-8")
				}
				index := -1
				for i, entry := range entries {
					if strings.Contains(entry, op.OldText) {
						if index != -1 {
							return errors.New("ambiguous old_text")
						}
						index = i
					}
				}
				if index == -1 {
					return errors.New("old_text not found")
				}
				if op.Action == "replace" {
					if err := validEntry(op.Content); err != nil {
						return err
					}
					entries[index] = op.Content
				} else {
					if op.Content != "" {
						return errors.New("remove cannot specify content")
					}
					entries = append(entries[:index], entries[index+1:]...)
				}
			default:
				return errors.New("invalid action")
			}
		}
		result = strings.Join(entries, "\n§\n")
		if utf8.RuneCountInString(result) > limit {
			return fmt.Errorf("target exceeds %d runes", limit)
		}
		return writeFile(ctx, dir, name, result)
	})
	return result, err
}

// Delete removes both private files while holding the user's lock. The lock
// file and directory remain to preserve cross-process lock identity.
func (s *Store) Delete(ctx context.Context, userID string) error {
	err := s.locked(ctx, userID, false, func(dir string) (removeErr error) {
		for _, name := range []string{"USER.md", "MEMORY.md"} {
			if _, err := readFile(dir, name, 2200); err != nil {
				return err
			}
		}
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		removed := false
		defer func() {
			if removed {
				removeErr = errors.Join(removeErr, d.Sync())
			}
		}()
		for _, name := range []string{"USER.md", "MEMORY.md"} {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			} else if err == nil {
				removed = true
			}
		}
		return nil
	})
	if errors.Is(err, errMissingUserDir) {
		return nil
	}
	return err
}

func targetFile(target string) (string, int, error) {
	switch target {
	case "user":
		return "USER.md", 1375, nil
	case "memory":
		return "MEMORY.md", 2200, nil
	default:
		return "", 0, errors.New("target must be user or memory")
	}
}

func validEntry(entry string) error {
	if !utf8.ValidString(entry) || strings.TrimSpace(entry) == "" {
		return errors.New("entry must be nonempty UTF-8")
	}
	for _, line := range strings.Split(entry, "\n") {
		if strings.TrimSuffix(line, "\r") == separator {
			return errors.New("entry contains delimiter")
		}
	}
	return nil
}

func split(content string) ([]string, error) {
	if content == "" {
		return nil, nil
	}
	entries := strings.Split(content, "\n§\n")
	for _, entry := range entries {
		if err := validEntry(entry); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func readFile(dir, name string, limit int) (string, error) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit*4+1)))
	if err != nil {
		return "", err
	}
	if len(data) > limit*4 || !utf8.Valid(data) || utf8.RuneCount(data) > limit {
		return "", errors.New("invalid or oversized file")
	}
	return string(data), nil
}

func writeFile(ctx context.Context, dir, name, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".memory-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) locked(ctx context.Context, userID string, create bool, fn func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !safeID.MatchString(userID) {
		return errors.New("invalid user ID")
	}
	if s == nil || s.root == "" {
		return errors.New("file store root is empty")
	}
	root, err := filepath.Abs(s.root)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, userID)
	// Reject symlinks at every ancestor, including a configured root symlink.
	path := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(dir, path), path) {
		if part == "" {
			continue
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return errMissingUserDir
			}
			if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(path)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe store directory")
		}
		if path == dir && info.Mode().Perm()&0077 != 0 {
			return errors.New("store directory is not private")
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe lock file")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn(dir)
}
