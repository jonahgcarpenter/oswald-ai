package skills

import (
	"fmt"
	"io"
	"strings"

	skillpkg "github.com/jonahgcarpenter/oswald-ai/internal/skills"
)

// removeCmd deletes one installed skill and its lockfile record. Skills are
// per-profile data, so no other profile can reference them; --full also
// removes that skill's backups.
func removeCmd(root, flagProfile string, rest []string, stdout, stderr io.Writer) int {
	var names []string
	full := false
	for _, token := range rest {
		switch token {
		case "--full":
			full = true
		default:
			if strings.HasPrefix(token, "-") {
				fmt.Fprintln(stderr, "usage: oswald skills remove <name> [--full] [--profile NAME]")
				return exitUsage
			}
			names = append(names, token)
		}
	}
	if len(names) != 1 {
		fmt.Fprintln(stderr, "usage: oswald skills remove <name> [--full] [--profile NAME]")
		return exitUsage
	}
	_, profileRoot, err := resolveScope(root, flagProfile, true)
	if err != nil {
		reportSkillsError(stderr, err)
		return exitError
	}
	if err := skillpkg.RemoveSkill(profileRoot, names[0], full); err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	name := strings.ToLower(strings.TrimSpace(names[0]))
	note := ""
	if full {
		note = " and its backups"
	}
	fmt.Fprintf(stdout, "removed %s%s\n", name, note)
	return exitOK
}
