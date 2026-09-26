package files

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Migrate moves legacy root/<userID> notes to root/<userID>/memories.
// Run it before serving requests and with all older processes stopped; it never
// overwrites destination files and can resume after a partial move.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	if s == nil || s.root == "" {
		return 0, errors.New("file store root is empty")
	}
	root, err := filepath.Abs(s.root)
	if err != nil {
		return 0, err
	}
	users, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, user := range users {
		if err := ctx.Err(); err != nil {
			return moved, err
		}
		if !safeID.MatchString(user.Name()) || !user.IsDir() {
			continue
		}
		oldDir := filepath.Join(root, user.Name())
		var names []string
		for _, name := range []string{"USER.md", "MEMORY.md"} {
			_, err := os.Lstat(filepath.Join(oldDir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return moved, err
			}
			names = append(names, name)
		}
		if len(names) == 0 {
			continue
		}
		err := s.withDir(ctx, user.Name(), true, func(dir string) error {
			for _, name := range names {
				limit := 2200
				if name == "USER.md" {
					limit = 1375
				}
				if _, err := readFile(oldDir, name, limit); err != nil {
					return err
				}
				if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
					return fmt.Errorf("legacy %s has an existing destination", name)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			for _, name := range names {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := os.Rename(filepath.Join(oldDir, name), filepath.Join(dir, name)); err != nil {
					return err
				}
				moved++
			}
			for _, path := range []string{oldDir, dir} {
				d, err := os.Open(path)
				if err != nil {
					return err
				}
				err = d.Sync()
				if closeErr := d.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return moved, fmt.Errorf("migrate private memory: %w", err)
		}
	}
	return moved, nil
}
