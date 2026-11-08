package skills

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	skillpkg "github.com/jonahgcarpenter/oswald-ai/internal/skills"
)

// infoCmd prints one skill's frontmatter, provenance, and required env.
// Values are never printed: only names, counts, and install metadata.
func infoCmd(root, flagProfile, raw string, stdout, stderr io.Writer) int {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		fmt.Fprintf(stderr, "oswald: invalid skill name %q\n", raw)
		return exitUsage
	}
	_, profileRoot, err := resolveScope(root, flagProfile, false)
	if err != nil {
		reportSkillsError(stderr, err)
		return exitError
	}
	store := skillpkg.NewStore(filepath.Join(profileRoot, "skills"))
	result, err := store.View(name, "", "")
	if err != nil {
		fmt.Fprintf(stderr, "oswald: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "name: %s\nload_name: %s\ncategory: %s\n", result.Name, result.LoadName, result.Category)
	version := result.Version
	if version == "" {
		version = "-"
	}
	fmt.Fprintf(stdout, "version: %s\ndescription: %s\n", version, result.Description)
	if len(result.RequiredEnv) > 0 {
		fmt.Fprintf(stdout, "requires_env: %s\n", strings.Join(result.RequiredEnv, ", "))
	} else {
		fmt.Fprintln(stdout, "requires_env: none")
	}
	entries, err := skillpkg.ReadLock(profileRoot)
	if err != nil {
		reportSkillsError(stderr, err)
		return exitError
	}
	if meta, ok := entries[result.Name]; ok {
		provenance := meta.Source
		if meta.Pinned && meta.Revision != "" {
			provenance += " pinned@" + shortSHA(meta.Revision)
		} else if meta.Revision != "" {
			provenance += " at " + shortSHA(meta.Revision)
		} else {
			provenance += " unpinned"
		}
		fmt.Fprintf(stdout, "installed_from: %s\n", provenance)
	} else {
		fmt.Fprintln(stdout, "installed_from: unmanaged")
	}
	return exitOK
}

func shortSHA(revision string) string {
	if len(revision) >= 8 {
		return revision[:8]
	}
	return revision
}
