// Package imagecache stores short-lived, private per-user image downloads.
package imagecache

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
	_ "golang.org/x/image/webp"
	"golang.org/x/sys/unix"
)

const lifetime = 24 * time.Hour

var userName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var imageName = regexp.MustCompile(`^[a-f0-9]{32}\.(png|jpg|gif|webp)$`)
var imageExtensions = [...]string{".png", ".jpg", ".gif", ".webp"}

var errUnsafeDirectory = errors.New("image cache directory is not private")

// Counts describes successful sweep removals.
type Counts struct {
	RemovedFiles int
	RemovedBytes int64
}

// Cache owns only files in root/<user>/.cache/images.
type Cache struct {
	root    string
	profile string
	now     func() time.Time
}

// New constructs a cache at root; use config.DefaultDataRoot for the application cache.
func New(root string) *Cache { return &Cache{root: root, now: time.Now} }

// NewProfileCache owns images directly below one manually provisioned profile.
func NewProfileCache(root, profile string) *Cache {
	return &Cache{root: root, profile: profile, now: time.Now}
}

func (c *Cache) rootPath() (string, error) {
	if c == nil || c.root == "" {
		return "", errors.New("image cache root is empty")
	}
	return filepath.Abs(c.root)
}

// directory opens each component without following symlinks. All subsequent
// file operations use the returned descriptor, not the checked pathname.
func (c *Cache) directory(ctx context.Context, user string, create bool) (int, string, error) {
	if !userName.MatchString(user) {
		return -1, "", errors.New("invalid user ID")
	}
	root, err := c.rootPath()
	if err != nil {
		return -1, "", err
	}
	path := filepath.Join(root, user, ".cache", "images")
	if c.profile != "" {
		if user != c.profile {
			return -1, "", errors.New("image cache profile mismatch")
		}
		profileFD, err := openDirectory(ctx, root, false, 1)
		if err != nil {
			return -1, "", err
		}
		unix.Close(profileFD)
		path = filepath.Join(root, ".cache", "images")
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

func validate(data []byte, declared string) (string, string, error) {
	if len(data) == 0 || len(data) > media.MaxOutputAttachmentBytes {
		return "", "", errors.New("image size out of bounds")
	}
	mime := http.DetectContentType(data)
	ext := ""
	switch mime {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	default:
		return "", "", errors.New("unsupported image signature")
	}
	if declared != "" && strings.TrimSpace(strings.ToLower(strings.Split(declared, ";")[0])) != mime {
		return "", "", errors.New("image MIME mismatch")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 || format != map[string]string{".png": "png", ".jpg": "jpeg", ".gif": "gif", ".webp": "webp"}[ext] {
		return "", "", errors.New("invalid image dimensions or encoding")
	}
	return mime, ext, nil
}

func token() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Save atomically publishes a validated image and returns its absolute cache path.
func (c *Cache) Save(ctx context.Context, userID string, data []byte, mime string) (string, error) {
	_, ext, err := validate(data, mime)
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

// SaveAsset stores an image under a stable, opaque name scoped to userID.
// A still-valid original wins over later normalized copies supplied for the
// same asset ID. Callers must use a distinct asset ID for distinct images.
func (c *Cache) SaveAsset(ctx context.Context, userID, assetID string, data []byte, mime string) (string, error) {
	if assetID == "" || len(assetID) > 4096 {
		return "", errors.New("invalid asset ID")
	}
	_, ext, err := validate(data, mime)
	if err != nil {
		return "", err
	}
	fd, dir, err := c.directory(ctx, userID, true)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	if err := lockDirectory(ctx, fd); err != nil {
		return "", err
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(assetID))
	base := hex.EncodeToString(hash[:16])
	var stale []string
	for _, suffix := range imageExtensions {
		name := base + suffix
		fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return "", err
		}
		f := os.NewFile(uintptr(fileFD), name)
		info, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return "", statErr
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			f.Close()
			return "", errors.New("unsafe cached asset")
		}
		now := c.now()
		if !info.ModTime().After(now) && now.Sub(info.ModTime()) < lifetime && info.Size() <= media.MaxOutputAttachmentBytes {
			original, readErr := io.ReadAll(io.LimitReader(f, media.MaxOutputAttachmentBytes+1))
			if readErr != nil {
				f.Close()
				return "", readErr
			}
			if _, actualExt, validationErr := validate(original, ""); validationErr == nil && actualExt == suffix {
				readErr = f.Close()
				if readErr != nil {
					return "", readErr
				}
				return filepath.Join(dir, name), nil
			}
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		stale = append(stale, name)
	}
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
	name := base + ext
	if err := unix.Renameat(fd, tmp, fd, name); err != nil {
		return "", err
	}
	for _, old := range stale {
		if old != name {
			if err := unix.Unlinkat(fd, old, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return "", err
			}
		}
	}
	if err := unix.Fsync(fd); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Resolve reads only an unexpired, private cache image owned by userID.
func (c *Cache) Resolve(ctx context.Context, userID, path string) ([]byte, string, error) {
	fd, dir, err := c.directory(ctx, userID, false)
	if err != nil {
		return nil, "", err
	}
	defer unix.Close(fd)
	name := filepath.Base(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != filepath.Join(dir, name) || !imageName.MatchString(name) {
		return nil, "", errors.New("path is not a user cache image")
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
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > media.MaxOutputAttachmentBytes || info.ModTime().After(now) || now.Sub(info.ModTime()) >= lifetime {
		return nil, "", errors.New("unsafe or expired image")
	}
	data, err := io.ReadAll(io.LimitReader(f, media.MaxOutputAttachmentBytes+1))
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	mime, ext, err := validate(data, "")
	if err != nil || !strings.HasSuffix(name, ext) {
		return nil, "", errors.New("invalid cached image")
	}
	return data, mime, nil
}

// Sweep removes expired cache images across user directories; it never descends
// into other directories or removes files not matching cache image names.
func (c *Cache) Sweep(ctx context.Context, now time.Time) (Counts, error) {
	var counts Counts
	if c != nil && c.profile != "" {
		err := c.walkImages(ctx, c.profile, func(fd int, name string, stat unix.Stat_t) error {
			if now.Sub(time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)) < lifetime {
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
	// Read via the validated directory descriptor, not the pathname.
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
		err = c.walkImages(ctx, user.Name(), func(fd int, name string, stat unix.Stat_t) error {
			if now.Sub(time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)) < lifetime {
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

// DeleteUser removes only this user's cache image files, leaving all other data intact.
func (c *Cache) DeleteUser(ctx context.Context, userID string) error {
	return c.walkImages(ctx, userID, func(fd int, name string, _ unix.Stat_t) error {
		return unix.Unlinkat(fd, name, 0)
	})
}

func (c *Cache) walkImages(ctx context.Context, user string, remove func(int, string, unix.Stat_t) error) error {
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
	d := os.NewFile(uintptr(dup), "images")
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
		if !imageName.MatchString(name) {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return fmt.Errorf("stat cache image: %w", err)
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
