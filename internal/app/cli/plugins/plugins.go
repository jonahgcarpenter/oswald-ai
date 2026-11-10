// Package plugins implements the `oswald plugins` command group: explicit,
// operator-driven management of per-profile plugins. Presence means enabled.
// All mutations are profile-fenced; reads never dial or execute plugin code.
package plugins

import (
	"context"
	"fmt"
	"io"
)

// Exit codes mirror the parent cli convention without importing it.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// Run dispatches the plugins command group. --profile is accepted in any position.
func Run(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitProfile(args)
	if len(rest) == 0 {
		pluginsUsage(stderr)
		return exitUsage
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald plugins list [--profile NAME]")
			return exitUsage
		}
		return listCmd(root, profile, stdout, stderr)
	case "info":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins info <name> [--profile NAME]")
			return exitUsage
		}
		return infoCmd(root, profile, rest[1], stdout, stderr)
	case "install":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins install <source> [--ref SHA] [--subdir PATH] [--node-package PKG --node-version VER] [--no-deps] [--force] [--profile NAME]")
			return exitUsage
		}
		return installCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "update":
		if len(rest) < 2 || len(rest) > 4 {
			fmt.Fprintln(stderr, "usage: oswald plugins update <name> [--ref SHA] [--profile NAME]")
			return exitUsage
		}
		return updateCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "remove":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins remove <name> [--full] [--profile NAME]")
			return exitUsage
		}
		return removeCmd(root, profile, rest[1:], stdout, stderr)
	case "install-deps":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins install-deps <name> [--profile NAME]")
			return exitUsage
		}
		return installDepsCmd(ctx, root, profile, rest[1], stdout, stderr)
	case "bin-path":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins bin-path <name> [--bin BIN] [--profile NAME]")
			return exitUsage
		}
		return binPathCmd(root, profile, rest[1:], stdout, stderr)
	case "exec":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins exec <name> [--bin BIN] [--timeout SECS] [-- <args...>] [--profile NAME]")
			return exitUsage
		}
		return execCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		pluginsUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown plugins command %q\n", rest[0])
		pluginsUsage(stderr)
		return exitUsage
	}
}

func pluginsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald plugins [--profile NAME] <list|info|install-deps|bin-path|exec|install|update|remove> [name]")
	fmt.Fprintln(w, "  list                  installed plugins (presence means enabled)")
	fmt.Fprintln(w, "  info <name>           manifest and entry state, secrets redacted")
	fmt.Fprintln(w, "  install-deps <name>   install pinned npm deps into the profile plugin home")
	fmt.Fprintln(w, "  bin-path <name>       print the absolute plugin-local binary path")
	fmt.Fprintln(w, "  exec <name>           run an installed plugin binary for the calling profile")
	fmt.Fprintln(w, "  install <source>      install a plugin from a git URL, owner/repo, or path (deps install too)")
	fmt.Fprintln(w, "  update <name>         refresh a managed install (pins need --ref)")
	fmt.Fprintln(w, "  remove <name>         drop a plugin and delete profile code (--full also deletes data)")
}
