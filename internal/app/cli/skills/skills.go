// Package skills implements the `oswald skills` command group: explicit,
// operator-driven installation of profile skills from git or local sources.
// Installs land in the calling profile's skills tree only; nothing is shared
// across profiles. Reads never execute skill code.
package skills

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

// Run dispatches the skills command group. --profile is accepted in any position.
func Run(ctx context.Context, root string, args []string, stdout, stderr io.Writer) int {
	profile, rest := splitProfile(args)
	if len(rest) == 0 {
		skillsUsage(stderr)
		return exitUsage
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: oswald skills list [--profile NAME]")
			return exitUsage
		}
		return listCmd(root, profile, stdout, stderr)
	case "info":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: oswald skills info <name> [--profile NAME]")
			return exitUsage
		}
		return infoCmd(root, profile, rest[1], stdout, stderr)
	case "install":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "usage: oswald skills install <source> [--ref SHA] [--subdir PATH] [--force] [--profile NAME]")
			return exitUsage
		}
		return installCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "update":
		if len(rest) < 2 || len(rest) > 4 {
			fmt.Fprintln(stderr, "usage: oswald skills update <name> [--ref SHA] [--force] [--profile NAME]")
			return exitUsage
		}
		return updateCmd(ctx, root, profile, rest[1:], stdout, stderr)
	case "remove":
		if len(rest) < 2 || len(rest) > 3 {
			fmt.Fprintln(stderr, "usage: oswald skills remove <name> [--full] [--profile NAME]")
			return exitUsage
		}
		return removeCmd(root, profile, rest[1:], stdout, stderr)
	case "help", "-h", "--help":
		skillsUsage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "oswald: unknown skills command %q\n", rest[0])
		skillsUsage(stderr)
		return exitUsage
	}
}

func skillsUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: oswald skills [--profile NAME] <list|info|install|update|remove> [name]")
	fmt.Fprintln(w, "  list                  installed skills with enable state")
	fmt.Fprintln(w, "  info <name>           manifest, provenance, and required env")
	fmt.Fprintln(w, "  install <source>      install a skill from a git URL, owner/repo, or path")
	fmt.Fprintln(w, "  update <name>         refresh a managed install (pins need --ref)")
	fmt.Fprintln(w, "  remove <name>         delete a skill and its lockfile record")
}
