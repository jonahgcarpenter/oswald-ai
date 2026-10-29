package skills

import (
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one loadable skill after catalog resolution. LoadName is what a
// view call accepts: the declared name when unique, otherwise the exact
// path relative to its root. Source names the owning tier.
type Skill struct {
	Name          string
	Description   string
	Version       string
	Platforms     []string
	Prerequisites []string
	Tags          []string
	Category      string
	LoadName      string
	Source        string
	Dir           string
	File          string
}

const (
	// SourceManaged marks skills under the profile's own skills tree.
	SourceManaged = "profile"
	// SourceExternal marks skills under the configured library directory.
	SourceExternal = "external"
)

// resolveCatalog turns scanned entries into the visible, loadable catalog
// for one platform (the gateway name, case-insensitive; "" matches skills
// without a platforms gate only when they declare none). Lower tiers win:
// an external copy shadowed by a managed one is dropped. Same-tier
// duplicates stay loadable by relative path; byte-identical directory
// repeats collapse.
func resolveCatalog(entries []RawEntry, platform string) []Skill {
	platform = strings.ToLower(strings.TrimSpace(platform))
	byName := make(map[string][]RawEntry)
	for _, entry := range entries {
		if !visibleOn(entry.Platforms, platform) {
			continue
		}
		byName[entry.Name] = append(byName[entry.Name], entry)
	}
	var out []Skill
	for name, group := range byName {
		out = append(out, resolveNameGroup(name, group)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].LoadName < out[j].LoadName
	})
	return out
}

// visibleOn reports whether a platforms gate admits this platform. An
// empty gate admits everything; a gated skill needs an exact
// case-insensitive platform match.
func visibleOn(platforms []string, platform string) bool {
	if len(platforms) == 0 {
		return true
	}
	for _, gated := range platforms {
		if strings.ToLower(gated) == platform {
			return true
		}
	}
	return false
}

// resolveNameGroup settles every entry sharing one declared name.
func resolveNameGroup(name string, group []RawEntry) []Skill {
	byDir := make(map[string]RawEntry, len(group))
	for _, entry := range group {
		if _, ok := byDir[entry.Dir]; !ok {
			byDir[entry.Dir] = entry
		}
	}
	entries := make([]RawEntry, 0, len(byDir))
	for _, entry := range byDir {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Tier != entries[j].Tier {
			return entries[i].Tier < entries[j].Tier
		}
		return entries[i].Dir < entries[j].Dir
	})
	if len(entries) == 1 {
		return []Skill{toSkill(entries[0], name, entries[0].Root)}
	}
	top := entries[0].Tier
	var winners []RawEntry
	for _, entry := range entries {
		if entry.Tier != top {
			break
		}
		winners = append(winners, entry)
	}
	if len(winners) == 1 {
		return []Skill{toSkill(winners[0], name, winners[0].Root)}
	}
	// Same-tier duplicates: each loads by its exact relative path.
	skills := make([]Skill, 0, len(winners))
	for _, entry := range winners {
		skill := toSkill(entry, name, entry.Root)
		if rel, err := filepath.Rel(entry.Root, entry.Dir); err == nil && rel != "." {
			skill.LoadName = filepath.ToSlash(rel)
		} else {
			skill.LoadName = name
		}
		skills = append(skills, skill)
	}
	return skills
}

// toSkill projects one entry, deriving the display category from its
// parent directory relative to its root ("general" at the top level).
func toSkill(entry RawEntry, loadName, root string) Skill {
	source := SourceExternal
	if entry.Tier == TierManaged {
		source = SourceManaged
	}
	return Skill{
		Name:          entry.Name,
		Description:   entry.Description,
		Version:       entry.Version,
		Platforms:     entry.Platforms,
		Prerequisites: entry.Prerequisites,
		Tags:          entry.Tags,
		Category:      categoryFor(entry.Dir, root),
		LoadName:      loadName,
		Source:        source,
		Dir:           entry.Dir,
		File:          entry.File,
	}
}

// categoryFor derives the display category from the skill directory's
// parent relative to its root.
func categoryFor(dir, root string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return "general"
	}
	parent := filepath.Dir(filepath.ToSlash(rel))
	if parent == "." {
		return "general"
	}
	return parent
}
