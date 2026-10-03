package profiles

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jonahgcarpenter/oswald-ai/internal/config"
)

// deleteProfile removes one profile subtree after an explicit confirmation. It
// refuses the reserved default profile and never follows symlinks.
func deleteProfile(ctx context.Context, root string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: oswald profile delete <name>")
		return exitUsage
	}
	name := args[0]
	if !config.ValidProfileName(name) {
		fmt.Fprintf(stderr, "invalid profile name %q\n", name)
		return exitUsage
	}
	absRoot, err := resolveRoot(root)
	if err != nil {
		fmt.Fprintf(stderr, "profile delete failed: %v\n", err)
		return exitError
	}
	dir, err := profileDir(absRoot, name)
	if err != nil {
		fmt.Fprintf(stderr, "profile delete failed: %v\n", err)
		return exitError
	}
	if err := assertDirectoryChain(filepath.Join(absRoot, "profiles")); err != nil {
		fmt.Fprintf(stderr, "profile delete failed: %v\n", err)
		return exitError
	}
	info, err := os.Lstat(dir)
	if err != nil {
		fmt.Fprintf(stderr, "profile %q does not exist\n", name)
		return exitError
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintf(stderr, "profile %q is not a safe directory\n", name)
		return exitError
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(stderr, "profile delete failed: %v\n", err)
		return exitError
	}
	warnProfileConsequences(absRoot, name, stdout, stderr)
	fmt.Fprintf(stdout, "Delete profile %q and all of its data? [y/N] ", name)
	if !confirm(stdin) {
		fmt.Fprintln(stderr, "aborted: profile not deleted")
		return exitError
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(stderr, "profile delete failed: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "deleted profile %q\n", name)
	return exitOK
}

// warnProfileConsequences reports configuration that will break once the
// profile is gone. It never edits operator configuration.
func warnProfileConsequences(root, name string, stdout, stderr io.Writer) {
	cfg, err := config.LoadProfiles(root)
	if err != nil {
		fmt.Fprintf(stderr, "warning: could not validate gateway routes: %v\n", err)
		return
	}
	for _, route := range cfg.ProfileRoutes {
		if route.Profile != name {
			continue
		}
		label := route.Name
		if label == "" {
			label = route.Platform
		}
		fmt.Fprintf(stdout, "warning: gateway route %q (%s) targets %q; startup fails until it is removed or repointed\n", label, route.Platform, name)
	}
}
