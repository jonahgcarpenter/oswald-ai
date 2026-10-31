package profiles

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
)

// rename moves one profile subtree to a new name with a single atomic
// directory move. Contents (state.db, .env, config.yaml, memories, cron)
// travel untouched, so no migration or rollback is needed. Profiles
// referenced by gateway.profile_routes refuse: startup fails on dangling
// routes, and routes must reference existing directories, so the supported
// order is strip the route entries from config.yaml, rename, then re-add
// them for the new name.
func renameProfile(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: oswald profile rename <old> <new>")
		return exitUsage
	}
	oldName, newName := args[0], args[1]
	if !config.ValidProfileName(oldName) {
		fmt.Fprintf(stderr, "invalid profile name %q\n", oldName)
		return exitUsage
	}
	if !config.ValidProfileName(newName) {
		fmt.Fprintf(stderr, "invalid profile name %q\n", newName)
		return exitUsage
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(stderr, "profile rename failed: %v\n", err)
		return exitError
	}
	absRoot, err := resolveRoot(root)
	if err != nil {
		fmt.Fprintf(stderr, "profile rename failed: %v\n", err)
		return exitError
	}
	oldDir, err := profileDir(absRoot, oldName)
	if err != nil {
		fmt.Fprintf(stderr, "profile rename failed: %v\n", err)
		return exitError
	}
	newDir, err := profileDir(absRoot, newName)
	if err != nil {
		fmt.Fprintf(stderr, "profile rename failed: %v\n", err)
		return exitError
	}
	info, err := os.Lstat(oldDir)
	if err != nil {
		fmt.Fprintf(stderr, "profile %q does not exist\n", oldName)
		return exitError
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintf(stderr, "profile %q is not a safe directory\n", oldName)
		return exitError
	}
	if _, err := os.Lstat(newDir); err == nil {
		fmt.Fprintf(stderr, "profile %q already exists\n", newName)
		return exitError
	}
	if err := refuseRoutedProfile(absRoot, oldName, stderr); err != nil {
		fmt.Fprintf(stderr, "profile rename failed: %v\n", err)
		return exitError
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		fmt.Fprintf(stderr, "profile rename failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "renamed profile %q to %q; restart the server to apply\n", oldName, newName)
	return exitOK
}

// refuseRoutedProfile fails when gateway routes still target the profile.
// Dangling routes fail startup, and routes must reference existing profile
// directories, so neither repoint-first (new dir missing) nor rename-first
// (dangling route) validates: the operator strips the route entries from
// config.yaml, renames, then re-adds them for the new name. List items are
// not addressable via `oswald config set`, so this is a direct config edit.
func refuseRoutedProfile(root, name string, stderr io.Writer) error {
	cfg, err := config.LoadProfiles(root)
	if err != nil {
		return fmt.Errorf("could not validate gateway routes: %v", err)
	}
	for _, route := range cfg.ProfileRoutes {
		if route.Profile != name {
			continue
		}
		label := route.Name
		if label == "" {
			label = route.Platform
		}
		fmt.Fprintf(stderr, "warning: gateway route %q (%s) targets %q; remove its gateway.profile_routes entry from config.yaml first, rename, then re-add it for the new name\n", label, route.Platform, name)
		return fmt.Errorf("profile %q is still routed", name)
	}
	return nil
}
