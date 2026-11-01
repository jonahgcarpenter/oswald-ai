package skills

import (
	"path/filepath"
	"sort"
)

// Skill is one loadable skill after catalog resolution. LoadName is what a
// view call accepts: the declared name when unique, otherwise the exact
// path relative to the skills root.
type Skill struct {
	Name        string
	Description string
	Version     string
	Category    string
	LoadName    string
	Dir         string
	File        string
}

// resolveCatalog turns scanned entries into the visible, loadable catalog.
// The platform argument is accepted for call compatibility but ignored:
// every skill is visible on every gateway. Same-named skills stay
// individually loadable by relative path.
func resolveCatalog(entries []RawEntry, _ string) []Skill {
	byName := make(map[string][]RawEntry)
	for _, entry := range entries {
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].Dir < entries[j].Dir })
	if len(entries) == 1 {
		return []Skill{toSkill(entries[0], name, entries[0].Root)}
	}
	// Same-named duplicates: each loads by its exact relative path.
	skills := make([]Skill, 0, len(entries))
	for _, entry := range entries {
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
// parent directory relative to the skills root ("general" at the top level).
func toSkill(entry RawEntry, loadName, root string) Skill {
	return Skill{
		Name:        entry.Name,
		Description: entry.Description,
		Version:     entry.Version,
		Category:    categoryFor(entry.Dir, root),
		LoadName:    loadName,
		Dir:         entry.Dir,
		File:        entry.File,
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
