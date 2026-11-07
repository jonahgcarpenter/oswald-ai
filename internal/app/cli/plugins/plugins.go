// Package plugins implements the `oswald plugins` command group: explicit,
// operator-driven listing and enablement of profile plugins. All mutations go
// through the mediated write path with staged validation; reads never dial or
// execute plugin code.
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
	case "enable":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins enable <name> [--profile NAME]")
			return exitUsage
		}
		return enableCmd(root, profile, rest[1], stdout, stderr)
	case "disable":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins disable <name> [--profile NAME]")
			return exitUsage
		}
		return disableCmd(root, profile, rest[1], stdout, stderr)
	case "info":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins info <name> [--profile NAME]")
			return exitUsage
		}
		return infoCmd(root, profile, rest[1], stdout, stderr)
	case "install":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: oswald plugins install <source> [--ref SHA] [--subdir PATH] [--enable|--no-enable] [--force] [--profile NAME]")
			return exitUsage
		}
		return installCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "update":
		if len(rest) < 2 || len(rest) > 3 {
			fmt.Fprintln(stderr, "usage: oswald plugins update <name> [--ref SHA]")
			return exitUsage
		}
		return updateCmd(ctx, root, rest[1:], stdout, stderr)
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
		if len(rest) < 2 || len(rest) > 3 {
			fmt.Fprintln(stderr, "usage: oswald plugins bin-path <name> [--bin BIN] [--profile NAME]")
			return exitUsage
		}
		return binPathCmd(root, profile, rest[1:], stdout, stderr)
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
	fmt.Fprintln(w, "usage: oswald plugins [--profile NAME] <list|enable|disable|info|install-deps|bin-path|install|update|remove> [name]")
	fmt.Fprintln(w, "  list                  discovered plugins with enable state")
	fmt.Fprintln(w, "  enable <name>         opt in to a discovered plugin")
	fmt.Fprintln(w, "  disable <name>        opt out; always wins over enable")
	fmt.Fprintln(w, "  info <name>           manifest and entry state, secrets redacted")
	fmt.Fprintln(w, "  install-deps <name>   install pinned npm deps into the profile plugin home")
	fmt.Fprintln(w, "  bin-path <name>       print the absolute plugin-local binary path")
	fmt.Fprintln(w, "  install <source>      install a plugin from a git URL, owner/repo, or path")
	fmt.Fprintln(w, "  update <name>         refresh a managed install (pins need --ref)")
	fmt.Fprintln(w, "  remove <name>         drop a plugin and delete shared code")
}
