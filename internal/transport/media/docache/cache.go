// Package docache stores short-lived, private per-user document downloads.
package docache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"golang.org/x/sys/unix"
)

const lifetime = 24 * time.Hour

var userName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var docName = regexp.MustCompile(`^[a-f0-9]{32}\.(pdf|txt|md|csv|log|json|docx|xlsx)$`)

var errUnsafeDirectory = errors.New("document cache directory is not private")

// Counts describes successful sweep removals.
type Counts struct {
	RemovedFiles int
	RemovedBytes int64
}

// Cache owns only files in root/<user>/cache/documents.
type Cache struct {
	root    string
	profile string
	now     func() time.Time
}

// New constructs a cache at root.
func New(root string) *Cache { return &Cache{root: root, now: time.Now} }

// NewProfileCache owns documents directly below one manually provisioned profile.
func NewProfileCache(root, profile string) *Cache {
	return &Cache{root: root, profile: profile, now: time.Now}
}

func (c *Cache) rootPath() (string, error) {
	if c == nil || c.root == "" {
		return "", errors.New("document cache root is empty")
	}
	return filepath.Abs(c.root)
}

func (c *Cache) directory(ctx context.Context, user string, create bool) (int, string, error) {
	if !userName.MatchString(user) {
		return -1, "", errors.New("invalid user ID")
	}
	root, err := c.rootPath()
	if err != nil {
		return -1, "", err
	}
	path := filepath.Join(root, user, "cache", "documents")
	if c.profile != "" {
		if user != c.profile {
			return -1, "", errors.New("document cache profile mismatch")
		}
		profileFD, err := openDirectory(ctx, root, false, 1)
		if err != nil {
			return -1, "", err
		}
		unix.Close(profileFD)
		path = filepath.Join(root, "cache", "documents")
	} else if user == "default" {
		path = filepath.Join(root, "cache", "documents")
	} else if info, err := os.Lstat(filepath.Join(root, "profiles", user)); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		path = filepath.Join(root, "profiles", user, "cache", "documents")
	}
	fd, err := openDirectory(ctx, path, create, 3)
	return fd, path, err
}

func openDirectory(ctx context.Context, path string, create bool, privateParts int) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		if err = ctx.Err(); err != nil {
			break
		}
		if part == "" {
			continue
		}
		if create {
			if e := unix.Mkdirat(fd, part, 0700); e != nil && e != unix.EEXIST {
				err = e
				break
			}
		}
		var next int
		next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			break
		}
		if i >= len(parts)-privateParts {
			var stat unix.Stat_t
			err = unix.Fstat(next, &stat)
			if err == nil && stat.Mode&0077 != 0 {
				err = errUnsafeDirectory
			}
			if err != nil {
				unix.Close(next)
				break
			}
		}
		unix.Close(fd)
		fd = next
	}
	if err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func validate(filename string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > media.MaxDocumentBytes {
		return "", errors.New("document size out of bounds")
	}
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(filename)))
	switch ext {
	case ".pdf", ".txt", ".md", ".csv", ".log", ".json", ".docx", ".xlsx":
	default:
		sniffed := http.DetectContentType(data)
		if !strings.HasPrefix(sniffed, "text/") {
			return "", errors.New("unsupported document type")
		}
		if ext == "" {
			ext = ".txt"
		} else {
			return "", errors.New("unsupported document type")
		}
	}
	return ext, nil
}

func token() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Save atomically publishes a validated document and returns its absolute cache path.
func (c *Cache) Save(ctx context.Context, userID, filename string, data []byte) (string, error) {
	ext, err := validate(filename, data)
	if err != nil {
		return "", err
	}
	fd, dir, err := c.directory(ctx, userID, true)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	id, err := token()
	if err != nil {
		return "", err
	}
	tmp := ".tmp-" + id
	out, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return "", err
	}
	defer unix.Unlinkat(fd, tmp, 0)
	f := os.NewFile(uintptr(out), tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name := id + ext
	if err := unix.Linkat(fd, tmp, fd, name, 0); err != nil {
		return "", err
	}
	if err := unix.Unlinkat(fd, tmp, 0); err != nil {
		return "", err
	}
	if err := unix.Fsync(fd); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Resolve reads only an unexpired, private cached document owned by userID.
func (c *Cache) Resolve(ctx context.Context, userID, path string) ([]byte, string, error) {
	fd, dir, err := c.directory(ctx, userID, false)
	if err != nil {
		return nil, "", err
	}
	defer unix.Close(fd)
	name := filepath.Base(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != filepath.Join(dir, name) || !docName.MatchString(name) {
		return nil, "", errors.New("path is not a user cache document")
	}
	fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	f := os.NewFile(uintptr(fileFD), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	now := c.now()
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > media.MaxDocumentBytes || info.ModTime().After(now) || now.Sub(info.ModTime()) >= lifetime {
		return nil, "", errors.New("unsafe or expired document")
	}
	data, err := io.ReadAll(io.LimitReader(f, media.MaxDocumentBytes+1))
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if len(data) == 0 || int64(len(data)) > media.MaxDocumentBytes {
		return nil, "", errors.New("invalid cached document")
	}
	mime := http.DetectContentType(data)
	if strings.HasSuffix(strings.ToLower(name), ".pdf") && !strings.Contains(strings.ToLower(mime), "pdf") && !strings.HasPrefix(mime, "text/") {
		mime = "application/pdf"
	}
	return data, mime, nil
}

// Sweep removes expired cached documents.
func (c *Cache) Sweep(ctx context.Context, now time.Time) (Counts, error) {
	var counts Counts
	if c != nil && c.profile != "" {
		err := c.walkDocs(ctx, c.profile, func(fd int, name string, stat unix.Stat_t) error {
			fallback := time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
			if now.Sub(prune.CreationTimeAt(fd, name, fallback, now)) < lifetime {
				return nil
			}
			if err := unix.Unlinkat(fd, name, 0); err != nil {
				return err
			}
			counts.RemovedFiles++
			counts.RemovedBytes += stat.Size
			return nil
		})
		return counts, err
	}
	root, err := c.rootPath()
	if err != nil {
		return counts, err
	}
	fd, err := openDirectory(ctx, root, false, 0)
	if errors.Is(err, os.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return counts, err
	}
	f := os.NewFile(uintptr(fd), root)
	users, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return counts, err
	}
	for _, user := range users {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if !userName.MatchString(user.Name()) || !user.IsDir() {
			continue
		}
		err = c.walkDocs(ctx, user.Name(), func(fd int, name string, stat unix.Stat_t) error {
			fallback := time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
			if now.Sub(prune.CreationTimeAt(fd, name, fallback, now)) < lifetime {
				return nil
			}
			if err := unix.Unlinkat(fd, name, 0); err != nil {
				return err
			}
			counts.RemovedFiles++
			counts.RemovedBytes += stat.Size
			return nil
		})
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, errUnsafeDirectory) {
			continue
		}
		if err != nil {
			return counts, err
		}
	}
	return counts, nil
}

// DeleteUser removes only this user's cached documents.
func (c *Cache) DeleteUser(ctx context.Context, userID string) error {
	return c.walkDocs(ctx, userID, func(fd int, name string, _ unix.Stat_t) error {
		return unix.Unlinkat(fd, name, 0)
	})
}

func (c *Cache) walkDocs(ctx context.Context, user string, remove func(int, string, unix.Stat_t) error) error {
	fd, _, err := c.directory(ctx, user, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := lockDirectory(ctx, fd); err != nil {
		return err
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	d := os.NewFile(uintptr(dup), "documents")
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return err
	}
	changed := false
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !docName.MatchString(name) {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return fmt.Errorf("stat cached document: %w", err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if err := remove(fd, name, stat); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return unix.Fsync(fd)
	}
	return nil
}

func lockDirectory(ctx context.Context, fd int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
