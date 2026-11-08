package skills

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	skillpkg "github.com/jonahgcarpenter/oswald-ai/internal/skills"
)

// listCmd prints one line per installed skill: load name, category, and the
// first-line description. Bundled plugin skills (read-through for enabled
// plugins) are marked with their plugin key.
func listCmd(root, flagProfile string, stdout, stderr io.Writer) int {
	name, profileRoot, err := resolveScope(root, flagProfile, false)
	if err != nil {
		reportSkillsError(stderr, err)
		return exitError
	}
	store := skillpkg.NewStore(filepath.Join(profileRoot, "skills"))
	found := store.List("")
	bundled := bundledSkills(mustAbs(root), name)
	if len(found) == 0 && len(bundled) == 0 {
		fmt.Fprintln(stdout, "no skills")
		return exitOK
	}
	seen := map[string]bool{}
	for _, skill := range found {
		seen[skill.LoadName] = true
		fmt.Fprintf(stdout, "%s [%s]: %s\n", skill.LoadName, skill.Category, skill.Description)
	}
	for _, entry := range bundled {
		if seen[entry.LoadName] {
			continue
		}
		fmt.Fprintf(stdout, "%s [%s]: %s (plugin %s)\n", entry.LoadName, entry.Category, entry.Description, entry.Source)
	}
	return exitOK
}

type bundledSkill struct {
	LoadName    string
	Category    string
	Description string
	Source      string
}

// bundledSkills lists read-through skills from enabled plugins for the
// profile. A broken config never blocks the profile's own skills listing.
func bundledSkills(globalRoot, profile string) []bundledSkill {
	cfg, err := config.Validate(globalRoot)
	if err != nil {
		return nil
	}
	var profileCfg *config.Config
	if profile == "" || profile == "default" {
		profileCfg = cfg
	} else {
		profileCfg = cfg.Profiles[profile]
		if profileCfg == nil {
			return nil
		}
	}
	var out []bundledSkill
	keys := append([]string(nil), profileCfg.PluginEnabled...)
	sort.Strings(keys)
	for _, key := range keys {
		blocked := false
		for _, denied := range profileCfg.PluginDisabled {
			if denied == key {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		dir := filepath.Join(globalRoot, "plugins-available", filepath.FromSlash(key), "skills")
		for _, entry := range skillpkg.ScanDir(dir) {
			out = append(out, bundledSkill{LoadName: key + "/" + entry.LoadName, Category: entry.Category, Description: entry.Description, Source: key})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LoadName < out[j].LoadName })
	return out
}

func mustAbs(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	return abs
}
