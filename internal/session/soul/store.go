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

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
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

// RepairProfile tightens permissions on existing soul paths without creating
// anything. Missing paths stay missing, symlinks and non-directories are left
// for the fail-closed checks in Read. Foreign-owned paths fail Chmod as
// non-root and are likewise left for Read to reject.
func RepairProfile(root, defaultSoul, profile string) {
	repairSoulDir(root, profile)
	repairSoulFile(filepath.Join(root, "SOUL.md"), "soul_file", profile)
	if defaultSoul != "" {
		repairSoulFile(defaultSoul, "soul_default_file", profile)
	}
}

func logSoulRepair(artifact string, before os.FileMode, profile string) {
	fields := []config.Field{config.F("config_source", artifact), config.F("old_mode", int(before.Perm()))}
	if profile != "" {
		fields = append(fields, config.F("profile", profile))
	}
	config.NewLogger(config.LevelWarn).Server("soul").Warn("soul.perms.repaired", "repaired private permissions", fields...)
}

func repairSoulDir(path, profile string) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode().Perm()&0077 == 0 {
		return
	}
	before := info.Mode()
	if err := os.Chmod(path, 0700); err != nil {
		return
	}
	logSoulRepair("soul_dir", before, profile)
}

func repairSoulFile(path, artifact, profile string) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Mode().Perm()&0077 == 0 {
		return
	}
	before := info.Mode()
	if err := os.Chmod(path, 0600); err != nil {
		return
	}
	logSoulRepair(artifact, before, profile)
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
