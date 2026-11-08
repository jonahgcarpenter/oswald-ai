package skills

import (
	"context"
	"fmt"
	"io"
	"strings"

	skillpkg "github.com/jonahgcarpenter/oswald-ai/internal/skills"
)

// installCmd installs a skill bundle into the calling profile's skills tree.
// Shared code stays shared: every install is per-profile. The explicit
// invocation is the consent gate; versions are exact pins so repeats resolve
// identically.
func installCmd(ctx context.Context, root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	var source, ref, subdir string
	force := false
	for i := 0; i < len(rest); i++ {
		token := rest[i]
		switch {
		case token == "--force":
			force = true
		case token == "--ref" && i+1 < len(rest):
			i++
			ref = rest[i]
		case strings.HasPrefix(token, "--ref="):
			ref = strings.TrimPrefix(token, "--ref=")
		case token == "--subdir" && i+1 < len(rest):
			i++
			subdir = rest[i]
		case strings.HasPrefix(token, "--subdir="):
			subdir = strings.TrimPrefix(token, "--subdir=")
		case strings.HasPrefix(token, "-"):
			fmt.Fprintln(stderr, "usage: oswald skills install <source> [--ref SHA] [--subdir PATH] [--force] [--profile NAME]")
			return exitUsage
		default:
			if source != "" {
				fmt.Fprintln(stderr, "usage: oswald skills install <source> [--ref SHA] [--subdir PATH] [--force] [--profile NAME]")
				return exitUsage
			}
			source = token
		}
	}
	if source == "" {
		fmt.Fprintln(stderr, "usage: oswald skills install <source> [--ref SHA] [--subdir PATH] [--force] [--profile NAME]")
		return exitUsage
	}
	_, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportSkillsError(stderr, err)
		return exitError
	}
	result, err := skillpkg.InstallSkill(ctx, profileRoot, source, ref, subdir, force)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	verb := "installed"
	if result.AlreadyInstalled {
		verb = "already installed"
	}
	pin := "unpinned"
	if result.Pinned {
		pin = "pinned@" + shortSHA(result.Revision)
	} else if result.Revision != "" {
		pin = "at " + shortSHA(result.Revision)
	}
	fmt.Fprintf(stdout, "%s %s (%s)\n", verb, result.Name, pin)
	return exitOK
}
