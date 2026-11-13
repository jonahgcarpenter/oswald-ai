// Package plugins implements the `oswald plugins` command group: explicit,
// operator-driven management of per-profile plugins. Presence means enabled.
// All mutations are profile-fenced; reads never dial or execute plugin code.
package plugins

import (
	"context"
	"fmt"
	"io"

	"github.com/jonahgcarpenter/oswald-ai/internal/app/cli/help"
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
	// Leaf usage lines double as `--help` pages: `oswald plugins <command> --help`
	// prints the same text to stdout and exits 0 before any scope checks.
	if len(rest) >= 2 && help.WantsHelp(rest[1:]) {
		switch rest[0] {
		case "list":
			fmt.Fprintln(stdout, listUsage)
			return exitOK
		case "info":
			fmt.Fprintln(stdout, infoUsage)
			return exitOK
		case "install":
			fmt.Fprintln(stdout, installUsage)
			return exitOK
		case "update":
			fmt.Fprintln(stdout, updateUsage)
			return exitOK
		case "remove":
			fmt.Fprintln(stdout, removeUsage)
			return exitOK
		}
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, listUsage)
			return exitUsage
		}
		return listCmd(root, profile, stdout, stderr)
	case "info":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, infoUsage)
			return exitUsage
		}
		return infoCmd(root, profile, rest[1], stdout, stderr)
	case "install":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, installUsage)
			return exitUsage
		}
		return installCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "update":
		if len(rest) < 2 || len(rest) > 4 {
			fmt.Fprintln(stderr, updateUsage)
			return exitUsage
		}
		return updateCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "remove":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, removeUsage)
			return exitUsage
		}
		return removeCmd(root, profile, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		pluginsUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown plugins command %q\n", rest[0])
		pluginsUsage(stderr)
		return exitUsage
	}
}

const (
	listUsage = "usage: oswald plugins list [--profile NAME]"
	infoUsage = "usage: oswald plugins info <name> [--profile NAME]"
)

func pluginsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald plugins [--profile NAME] <list|info|install|update|remove> [name]")
	fmt.Fprintln(w, "  list                  installed plugins (presence means enabled)")
	fmt.Fprintln(w, "  info <name>           manifest and entry state, secrets redacted")
	fmt.Fprintln(w, "  install <source>      install a plugin from a git URL, owner/repo, or path (deps install too)")
	fmt.Fprintln(w, "  update <name>         refresh a managed install (pins need --ref)")
	fmt.Fprintln(w, "  remove <name>         drop a plugin and delete profile code (--full also deletes data)")
}
