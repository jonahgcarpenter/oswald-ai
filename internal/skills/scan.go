package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Tier precedence for same-named skills: the profile-managed tree always
// beats the external library.
const (
	TierManaged = iota
	TierExternal
)

// RawEntry is one SKILL.md found on disk, before catalog resolution.
type RawEntry struct {
	Name          string
	Description   string
	Version       string
	Platforms     []string
	Prerequisites []string
	Tags          []string
	Dir           string
	File          string
	Tier          int
	Root          string
}

// scanRoots walks the managed and external roots (either may be empty or
// missing) and returns every readable SKILL.md in stable sorted order.
// Malformed files are skipped; a scan never fails because of one file.
func scanRoots(managed, external string) []RawEntry {
	var out []RawEntry
	for _, root := range []struct {
		dir  string
		tier int
	}{{managed, TierManaged}, {external, TierExternal}} {
		abs := strings.TrimSpace(root.dir)
		if abs == "" {
			continue
		}
		out = append(out, scanRoot(abs, root.tier)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

// scanRoot returns the skills directly under one root. Directory symlinks
// are not followed; files over the read cap are skipped.
func scanRoot(root string, tier int) []RawEntry {
	var out []RawEntry
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "SKILL.md" {
			return nil
		}
		if info, err := entry.Info(); err != nil || info.Size() > maxSkillFileBytes {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		front, body, ok := parseFrontmatter(string(raw))
		if !ok {
			return nil
		}
		dir := filepath.Dir(path)
		name := strings.TrimSpace(front.name)
		if name == "" || len([]rune(name)) > maxSkillNameRunes {
			name = filepath.Base(dir)
		}
		description := clipRunes(front.description, maxDescriptionRunes)
		if description == "" {
			description = fallbackDescription(body)
		}
		out = append(out, RawEntry{
			Name:          name,
			Description:   description,
			Version:       strings.TrimSpace(front.version),
			Platforms:     normalizedList(front.platforms),
			Prerequisites: normalizedList(front.prerequisites),
			Tags:          normalizedList(front.tags),
			Dir:           dir,
			File:          path,
			Tier:          tier,
			Root:          root,
		})
		return nil
	})
	return out
}

// normalizedList trims, drops empties, and dedupes a metadata list.
func normalizedList(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
