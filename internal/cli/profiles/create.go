package profiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
	"github.com/jonahgcarpenter/oswald-ai/internal/database"
)

// create provisions one new private profile directory. It never touches an
// existing profile and rolls back partial state on failure.
func create(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: oswald profile create <name>")
		return exitUsage
	}
	name := args[0]
	if !config.ValidProfileName(name) {
		fmt.Fprintf(stderr, "invalid profile name %q\n", name)
		return exitUsage
	}
	if err := runCreate(ctx, root, name); err != nil {
		fmt.Fprintf(stderr, "profile create failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "created profile %q\n", name)
	return exitOK
}

// openProfileState is a seam over the approved-schema initializer so tests can
// exercise rollback without touching an operator database.
var openProfileState = func(ctx context.Context, path string) error {
	db, err := database.OpenState(ctx, path, nil)
	if err != nil {
		return err
	}
	return db.Close()
}

func runCreate(ctx context.Context, root, name string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	absRoot, err := resolveRoot(root)
	if err != nil {
		return fmt.Errorf("configuration root: %w", err)
	}
	// Validate every prerequisite before creating operator-visible state.
	soul, err := readPrivateFile(filepath.Join(absRoot, "SOUL.md"), maxSoulBytes)
	if err != nil {
		return fmt.Errorf("read default SOUL.md: %w", err)
	}
	dir, err := profileDir(absRoot, name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("profile %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	profilesRoot := filepath.Join(absRoot, "profiles")
	if err := os.Mkdir(profilesRoot, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	// The parent container only needs to be a real directory; each profile
	// directory itself is created private. This matches the runtime contract.
	if err := assertDirectoryChain(profilesRoot); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	created := true
	defer func() {
		if created {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := writePrivateFile(filepath.Join(dir, ".env"), nil); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dir, "config.yaml"), []byte(defaultProfileConfig)); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dir, "SOUL.md"), soul); err != nil {
		return err
	}
	if err := openProfileState(ctx, filepath.Join(dir, "state.db")); err != nil {
		return err
	}
	created = false
	return nil
}
