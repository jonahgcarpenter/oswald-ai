package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Store binds the profile's managed skills tree. Reads scan fresh on every
// call, so edits are visible immediately with no cache to invalidate.
type Store struct {
	managed string
}

// NewStore binds the managed skills root. An empty or missing directory
// reads as empty rather than failing.
func NewStore(managed string) *Store {
	return &Store{managed: managed}
}

// List returns the visible, resolved catalog for one platform.
func (s *Store) List(platform string) []Skill {
	if s == nil {
		return nil
	}
	return resolveCatalog(scanRoots(s.managed), platform)
}

// Index renders the prompt selection block for one platform, or "" when
// there is nothing to list.
func (s *Store) Index(platform string) string {
	return RenderIndex(s.List(platform))
}

// View loads one skill by name for one platform.
func (s *Store) View(name, filePath, platform string) (ViewResult, error) {
	if s == nil {
		return ViewResult{}, fmt.Errorf("skill store is unavailable")
	}
	return View(scanRoots(s.managed), name, filePath, platform)
}

// skillNameRE bounds created skill names to filesystem-safe slugs.
var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// validateNewName rejects traversal, hidden entries, and SKILL.md itself.
func validateNewName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !skillNameRE.MatchString(name) || name == "skill.md" {
		return "", fmt.Errorf("skill name must be 1-64 lowercase letters, digits, - or _, starting with a letter or digit")
	}
	return name, nil
}

// Create writes a new SKILL.md under the managed tree. An existing skill
// errors instead of overwriting; update it instead.
func (s *Store) Create(name, description, body string) (Skill, error) {
	if s == nil || strings.TrimSpace(s.managed) == "" {
		return Skill{}, fmt.Errorf("skill storage is unavailable")
	}
	name, err := validateNewName(name)
	if err != nil {
		return Skill{}, err
	}
	description = clipRunes(description, maxDescriptionRunes)
	if strings.TrimSpace(description) == "" {
		return Skill{}, fmt.Errorf("skill description is required")
	}
	if strings.TrimSpace(body) == "" {
		return Skill{}, fmt.Errorf("skill body is required")
	}
	dir := filepath.Join(s.managed, name)
	if info, err := os.Lstat(dir); err == nil {
		_ = info
		return Skill{}, fmt.Errorf("skill %q already exists", name)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Skill{}, fmt.Errorf("skill storage is unavailable")
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\nversion: 1.0.0\n---\n\n%s\n", name, description, strings.TrimSpace(body))
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0600); err != nil {
		return Skill{}, fmt.Errorf("skill storage is unavailable")
	}
	for _, skill := range resolveCatalog(scanRoots(s.managed), "") {
		if skill.LoadName == name {
			return skill, nil
		}
	}
	return Skill{Name: name, Description: description, Category: "general", LoadName: name, Dir: dir}, nil
}

// Update replaces one substring of a skill's SKILL.md, identified by an
// exact unique old_text match (the same contract as memory entry edits).
func (s *Store) Update(name, oldText, newContent string) (Skill, error) {
	if s == nil || strings.TrimSpace(s.managed) == "" {
		return Skill{}, fmt.Errorf("skill storage is unavailable")
	}
	if strings.TrimSpace(oldText) == "" {
		return Skill{}, fmt.Errorf("old_text is required: the exact unique substring to replace")
	}
	view, err := View(scanRoots(s.managed), name, "", "")
	if err != nil {
		return Skill{}, err
	}
	raw, err := os.ReadFile(view.File)
	if err != nil {
		return Skill{}, fmt.Errorf("skill storage is unavailable")
	}
	content := string(raw)
	if strings.Count(content, oldText) != 1 {
		return Skill{}, fmt.Errorf("old_text must match exactly once in %q", view.LoadName)
	}
	updated := strings.Replace(content, oldText, newContent, 1)
	info, err := os.Lstat(view.File)
	if err != nil {
		return Skill{}, fmt.Errorf("skill storage is unavailable")
	}
	if err := os.WriteFile(view.File, []byte(updated), info.Mode().Perm()); err != nil {
		return Skill{}, fmt.Errorf("skill storage is unavailable")
	}
	for _, skill := range resolveCatalog(scanRoots(s.managed), "") {
		if skill.File == view.File {
			return skill, nil
		}
	}
	return view.Skill, nil
}
