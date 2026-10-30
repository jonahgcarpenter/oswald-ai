package config

import (
	"errors"
	"os"
	"path/filepath"
)

// MaxConfigBytes bounds configuration and dotenv files, matching the runtime
// read limit.
const MaxConfigBytes = 256 * 1024

// LastKnownGoodPath returns the private undo target alongside a config or env
// file. It lives in the same directory so the writer's atomic rename keeps
// readers on a stable file either way.
func LastKnownGoodPath(path string) string { return path + ".last-known-good" }

// AtomicWritePrivateFile replaces path via a same-directory temp file, fsync,
// and rename so a watcher or concurrent reader never sees a partial file. An
// existing file must be a regular non-symlink; a missing file is created
// owner-only. Data is bounded to MaxConfigBytes.
func AtomicWritePrivateFile(path string, data []byte) error {
	if len(data) > MaxConfigBytes {
		return errors.New("configuration file exceeds size limit")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return errors.New("configuration file unavailable")
	}
	if info, err := os.Lstat(abs); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("configuration file unavailable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("configuration file unavailable")
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return errors.New("configuration file unavailable")
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return errors.New("configuration file unavailable")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return errors.New("configuration file unavailable")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("configuration file unavailable")
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return errors.New("configuration file unavailable")
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return errors.New("configuration file unavailable")
	}
	tmpName = ""
	return nil
}

// SaveLastKnownGood keeps the previous bytes as the undo target.
func SaveLastKnownGood(path string, data []byte) error {
	return AtomicWritePrivateFile(LastKnownGoodPath(path), data)
}

// ReadLastKnownGood returns the undo target, or os.ErrNotExist when no
// successful write has been recorded yet.
func ReadLastKnownGood(path string) ([]byte, error) {
	data, err := os.ReadFile(LastKnownGoodPath(path))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxConfigBytes+1 {
		return nil, errors.New("configuration file exceeds size limit")
	}
	return data, nil
}
