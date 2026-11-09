package skills

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/platform/config"
	pluginspkg "github.com/jonahgcarpenter/oswald-ai/internal/plugins"
	skillpkg "github.com/jonahgcarpenter/oswald-ai/internal/skills"
)

// listCmd prints one line per installed skill: load name, category, and the
// first-line description. Installed plugin skills (read-through for present
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

// bundledSkills lists read-through skills from installed profile plugins.
// Presence means enabled. A broken config never blocks the profile's own
// skills listing.
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
	if profileCfg == nil || strings.TrimSpace(profileCfg.ProfileRoot) == "" {
		return nil
	}
	var out []bundledSkill
	for _, m := range pluginspkg.ScanProfilePlugins(profileCfg.ProfileRoot) {
		dir := filepath.Join(pluginspkg.PluginCodeDir(profileCfg.ProfileRoot, m.Key), "skills")
		for _, entry := range skillpkg.ScanDir(dir) {
			out = append(out, bundledSkill{LoadName: m.Key + "/" + entry.LoadName, Category: entry.Category, Description: entry.Description, Source: m.Key})
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
