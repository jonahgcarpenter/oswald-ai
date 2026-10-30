package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Bounds for loaded skill content. Bodies ride tool results, whose own
// history envelope caps near the same size.
const (
	// maxBodyRunes caps one loaded skill body or supporting file.
	maxBodyRunes = 16000
	// maxSupportingFiles caps the supporting-file walk per skill.
	maxSupportingFiles = 64
)

// supportingDirs are the conventional subdirectories listed (and readable)
// alongside a skill body.
var supportingDirs = []string{"references", "templates", "scripts", "assets"}

// ViewResult is one fully loaded skill.
type ViewResult struct {
	Skill
	Body        string
	Supporting  []string
	FilePath    string
	FileContent string
	Truncated   bool
}

// View loads one skill by load name, declared name, directory name, or
// relative path (exact match first, then case-insensitive), managed tier
// before external. An unknown name errors without guessing; a name shared
// by same-tier duplicates errors listing the exact paths to disambiguate.
// filePath reads one supporting file inside the skill directory instead of
// the body; it must stay inside the skill directory.
func View(entries []RawEntry, name, filePath, platform string) (ViewResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ViewResult{}, fmt.Errorf("skill name is required")
	}
	catalog := resolveCatalog(entries, platform)
	candidates := matchSkills(catalog, name)
	if len(candidates) == 0 {
		return ViewResult{}, fmt.Errorf("unknown skill %q", name)
	}
	if len(candidates) > 1 {
		paths := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			paths = append(paths, candidate.LoadName)
		}
		sort.Strings(paths)
		return ViewResult{}, fmt.Errorf("ambiguous skill name %q: use one of %s", name, strings.Join(paths, ", "))
	}
	skill := candidates[0]
	result := ViewResult{Skill: skill, Supporting: supportingFiles(skill.Dir)}
	if trimmed := strings.TrimSpace(filePath); trimmed != "" {
		content, err := readSupportingFile(skill.Dir, trimmed)
		if err != nil {
			return ViewResult{}, err
		}
		result.FilePath = trimmed
		result.FileContent = content
		return result, nil
	}
	body, truncated, err := readBody(skill.File)
	if err != nil {
		return ViewResult{}, err
	}
	result.Body = body
	result.Truncated = truncated
	return result, nil
}

// matchSkills resolves a lookup name against the catalog: load names and
// declared names first (exact, then case-insensitive), then directory base
// names as a last resort.
func matchSkills(catalog []Skill, name string) []Skill {
	for _, pass := range []func(Skill) string{
		func(skill Skill) string { return skill.LoadName },
		func(skill Skill) string { return skill.Name },
	} {
		var exact, folded []Skill
		for _, skill := range catalog {
			switch {
			case pass(skill) == name:
				exact = append(exact, skill)
			case strings.EqualFold(pass(skill), name):
				folded = append(folded, skill)
			}
		}
		if len(exact) > 0 {
			return exact
		}
		if len(folded) > 0 {
			return folded
		}
	}
	var matches []Skill
	for _, skill := range catalog {
		if skill.Name != skill.LoadName {
			continue
		}
		if filepath.Base(skill.Dir) == name || strings.EqualFold(filepath.Base(skill.Dir), name) {
			matches = append(matches, skill)
		}
	}
	return matches
}

// readBody returns the SKILL.md body (frontmatter stripped), clipped with
// a marker when it exceeds its budget.
func readBody(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("skill body is unavailable")
	}
	if len(raw) > maxSkillFileBytes {
		raw = raw[:maxSkillFileBytes]
	}
	_, body, ok := parseFrontmatter(string(raw))
	if !ok {
		return "", false, fmt.Errorf("skill body is unavailable")
	}
	if utf8.RuneCountInString(body) <= maxBodyRunes {
		return body, false, nil
	}
	runes := []rune(body)
	return string(runes[:maxBodyRunes]) + fmt.Sprintf("\n\n[...truncated: showing %d of %d chars; read supporting files individually with file_path.]", maxBodyRunes, len(runes)), true, nil
}

// supportingFiles lists skill-relative support paths under the
// conventional subdirectories, in stable order.
func supportingFiles(dir string) []string {
	var out []string
	for _, sub := range supportingDirs {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if len(out) >= maxSupportingFiles {
				return out
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				continue
			}
			out = append(out, filepath.ToSlash(filepath.Join(sub, entry.Name())))
		}
	}
	sort.Strings(out)
	return out
}

// readSupportingFile reads one skill-relative file, confined to the skill
// directory and capped like a body.
func readSupportingFile(dir, filePath string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(filePath))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("file_path must stay inside the skill directory")
	}
	abs := filepath.Join(dir, clean)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file %q not found in this skill", clean)
		}
		return "", fmt.Errorf("file %q is unavailable", clean)
	}
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("file %q is unavailable", clean)
	}
	if resolved != base && !strings.HasPrefix(resolved, base+string(filepath.Separator)) {
		return "", fmt.Errorf("file_path must stay inside the skill directory")
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("file %q is unavailable", clean)
	}
	if len(raw) > maxSkillFileBytes {
		raw = raw[:maxSkillFileBytes]
	}
	if utf8.RuneCountInString(string(raw)) > maxBodyRunes {
		runes := []rune(string(raw))
		return string(runes[:maxBodyRunes]) + fmt.Sprintf("\n\n[...truncated: showing %d of %d chars.]", maxBodyRunes, len(runes)), nil
	}
	return string(raw), nil
}
