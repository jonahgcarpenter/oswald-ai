package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	Root          string
}

// scanRoots walks the managed skills root (empty or missing reads as
// empty) and returns every readable SKILL.md in stable sorted order.
// Malformed files are skipped; a scan never fails because of one file.
func scanRoots(managed string) []RawEntry {
	abs := strings.TrimSpace(managed)
	if abs == "" {
		return nil
	}
	out := scanRoot(abs)
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// scanRoot returns the skills directly under one root. Directory symlinks
// are not followed; files over the read cap are skipped.
func scanRoot(root string) []RawEntry {
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
