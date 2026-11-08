package skills

import (
	"context"
	"fmt"
	"io"
	"strings"

	skillpkg "github.com/jonahgcarpenter/oswald-ai/internal/skills"
)

// updateCmd refreshes one managed skill install in place. Pinned installs need
// an explicit --ref; unpinned installs follow the remote HEAD. Operator edits
// refuse without --force, which backs the modified tree up first.
func updateCmd(ctx context.Context, root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	var names []string
	var ref string
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
		case strings.HasPrefix(token, "-"):
			fmt.Fprintln(stderr, "usage: oswald skills update <name> [--ref SHA] [--force] [--profile NAME]")
			return exitUsage
		default:
			names = append(names, token)
		}
	}
	if len(names) != 1 {
		fmt.Fprintln(stderr, "usage: oswald skills update <name> [--ref SHA] [--force] [--profile NAME]")
		return exitUsage
	}
	_, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportSkillsError(stderr, err)
		return exitError
	}
	result, err := skillpkg.UpdateSkill(ctx, profileRoot, names[0], ref, force)
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	pin := "unpinned"
	if result.Pinned {
		pin = "pinned@" + shortSHA(result.Revision)
	}
	fmt.Fprintf(stdout, "updated %s (%s)\n", result.Name, pin)
	return exitOK
}
