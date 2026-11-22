// Package docache stores short-lived, private per-user inbound file
// downloads: documents (extractable text) and motion media (GIF/video).
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

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/platform/prune"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
	"golang.org/x/sys/unix"
)

const lifetime = 24 * time.Hour

// kind selects the owned subdirectory, extension set, and size cap.
type kind int

const (
	kindDocuments kind = iota
	kindMotion
)

var userName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var docName = regexp.MustCompile(`^[a-f0-9]{32}\.(pdf|txt|md|csv|log|json|docx|xlsx)$`)
var motionName = regexp.MustCompile(`^[a-f0-9]{32}\.(gif|mp4|webm|mov|m4v|mkv)$`)

var documentExtensions = map[string]struct{}{
	".pdf": {}, ".txt": {}, ".md": {}, ".csv": {}, ".log": {}, ".json": {}, ".docx": {}, ".xlsx": {},
}

var motionExtensions = map[string]struct{}{
	".gif": {}, ".mp4": {}, ".webm": {}, ".mov": {}, ".m4v": {}, ".mkv": {},
}

var errUnsafeDirectory = errors.New("document cache directory is not private")

// Counts describes successful sweep removals.
type Counts struct {
	RemovedFiles int
	RemovedBytes int64
}

// Cache owns only files in root/<user>/cache/<kind dir>.
type Cache struct {
	root    string
	profile string
	kind    kind
	now     func() time.Time
}

// New constructs a document cache at root.
func New(root string) *Cache { return &Cache{root: root, now: time.Now} }

// NewMotion constructs a motion-media cache at root.
func NewMotion(root string) *Cache {
	return &Cache{root: root, kind: kindMotion, now: time.Now}
}

// NewProfileCache owns documents directly below one manually provisioned profile.
func NewProfileCache(root, profile string) *Cache {
	return &Cache{root: root, profile: profile, now: time.Now}
}

// NewProfileMotionCache owns motion media directly below one manually
// provisioned profile.
func NewProfileMotionCache(root, profile string) *Cache {
	return &Cache{root: root, profile: profile, kind: kindMotion, now: time.Now}
}

func (c *Cache) subdir() string {
	if c != nil && c.kind == kindMotion {
		return "videos"
	}
	return "documents"
}

func (c *Cache) namePattern() *regexp.Regexp {
	if c != nil && c.kind == kindMotion {
		return motionName
	}
	return docName
}

func (c *Cache) extensions() map[string]struct{} {
	if c != nil && c.kind == kindMotion {
		return motionExtensions
	}
	return documentExtensions
}

func (c *Cache) maxBytes() int {
	if c != nil && c.kind == kindMotion {
		return media.MaxMotionBytes
	}
	return media.MaxDocumentBytes
}

func logCacheRepair(profile, artifact string, before os.FileMode) {
	config.NewLogger(config.LevelWarn).Server("docache").Warn("docache.perms.repaired", "repaired private permissions", config.F("config_source", artifact), config.F("old_mode", int(before.Perm())), config.F("profile", profile))
}

// RepairProfile tightens permissions on existing cached documents without
// creating anything. Missing paths stay missing; symlinks and foreign-owned
// paths are left for the fail-closed checks.
func RepairProfile(root, profile string) {
	repairKind(root, profile, kindDocuments)
}

// RepairMotionProfile tightens permissions on existing cached motion media.
func RepairMotionProfile(root, profile string) {
	repairKind(root, profile, kindMotion)
}

func repairKind(root, profile string, k kind) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(profile) == "" {
		return
	}
	c := &Cache{root: root, profile: profile, kind: k}
	dir := filepath.Join(root, "cache", c.subdir())
	if info, err := os.Lstat(dir); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0077 != 0 {
		before := info.Mode()
		if err := os.Chmod(dir, 0700); err == nil {
			logCacheRepair(profile, c.subdir()+"_dir", before)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !c.namePattern().MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 == 0 {
			continue
		}
		before := info.Mode()
		if err := os.Chmod(path, 0600); err != nil {
			continue
		}
		logCacheRepair(profile, c.subdir()+"_file", before)
	}
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
	path := filepath.Join(root, user, "cache", c.subdir())
	if c.profile != "" {
		if user != c.profile {
			return -1, "", errors.New("document cache profile mismatch")
		}
		profileFD, err := openDirectory(ctx, root, false, 1)
		if err != nil {
			return -1, "", err
		}
		unix.Close(profileFD)
		path = filepath.Join(root, "cache", c.subdir())
	} else if user == "default" {
		path = filepath.Join(root, "cache", c.subdir())
	} else if info, err := os.Lstat(filepath.Join(root, "profiles", user)); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		path = filepath.Join(root, "profiles", user, "cache", c.subdir())
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

func (c *Cache) validate(filename string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > c.maxBytes() {
		return "", errors.New("file size out of bounds")
	}
	ext := strings.ToLower(strings.TrimSpace(filepath.Ext(filename)))
	if _, ok := c.extensions()[ext]; ok {
		return ext, nil
	}
	if c.kind == kindMotion {
		return "", errors.New("unsupported motion media type")
	}
	sniffed := http.DetectContentType(data)
	if !strings.HasPrefix(sniffed, "text/") {
		return "", errors.New("unsupported document type")
	}
	if ext == "" {
		return ".txt", nil
	}
	return "", errors.New("unsupported document type")
}

func token() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Save atomically publishes a validated file and returns its absolute cache path.
func (c *Cache) Save(ctx context.Context, userID, filename string, data []byte) (string, error) {
	ext, err := c.validate(filename, data)
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
	if !filepath.IsAbs(path) || filepath.Clean(path) != filepath.Join(dir, name) || !c.namePattern().MatchString(name) {
		return nil, "", errors.New("path is not a user cache file")
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
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > int64(c.maxBytes()) || info.ModTime().After(now) || now.Sub(info.ModTime()) >= lifetime {
		return nil, "", errors.New("unsafe or expired document")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(c.maxBytes())+1))
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if len(data) == 0 || len(data) > c.maxBytes() {
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
		if !c.namePattern().MatchString(name) {
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
