package skills

import (
	"strings"
	"unicode/utf8"
)

// Bounds for selection metadata. Bodies have their own cap in read.go.
const (
	// maxDescriptionRunes caps one skill's index description at 60 chars.
	maxDescriptionRunes = 60
	// maxSkillNameRunes bounds a declared skill name.
	maxSkillNameRunes = 64
	// maxSkillFileBytes caps how much of one SKILL.md is ever read.
	maxSkillFileBytes = 1 << 20
)

// parsedSkill is the frontmatter subset the catalog understands: name,
// description, and version only. Every other key (author, license,
// platforms, tags, metadata, etc.) is ignored so libraries carrying
// richer metadata keep working.
type parsedSkill struct {
	name        string
	description string
	version     string
}

// parseFrontmatter splits raw SKILL.md content into frontmatter and body.
// Without a leading "---" fence the whole file is the body and the name
// falls back to the directory name at scan time. An unclosed fence fails
// closed: the file is skipped by returning ok=false.
func parseFrontmatter(content string) (front parsedSkill, body string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return parsedSkill{}, strings.TrimSpace(content), true
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return parsedSkill{}, "", false
	}
	front = parseFrontmatterKeys(lines[1:end])
	return front, strings.TrimSpace(strings.Join(lines[end+1:], "\n")), true
}

// parseFrontmatterKeys reads flat top-level "key: value" pairs for name,
// description, and version only. Only lines starting at column 0 count as
// keys: indented lines (nested maps like metadata:, list items) are ignored
// entirely. Values may be bare or single/double quoted; full-line "#"
// comments and trailing comments outside quotes are dropped.
func parseFrontmatterKeys(lines []string) parsedSkill {
	var out parsedSkill
	for _, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = unquote(strings.TrimSpace(cutComment(value)))
		switch key {
		case "name":
			out.name = value
		case "description":
			out.description = value
		case "version":
			out.version = value
		}
	}
	return out
}

// cutComment drops a trailing comment: a "#" outside quotes that starts
// the value or follows whitespace.
func cutComment(value string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
				return strings.TrimRight(value[:i], " \t")
			}
		}
	}
	return value
}

// unquote strips one layer of matching single or double quotes.
func unquote(value string) string {
	if len(value) >= 2 {
		if first, last := value[0], value[len(value)-1]; (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// fallbackDescription is the first non-empty, non-heading body line.
func fallbackDescription(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return clipRunes(trimmed, maxDescriptionRunes)
		}
	}
	return ""
}

// clipRunes shortens value to at most max runes, appending "..." if cut.
func clipRunes(value string, max int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	return string([]rune(value)[:max]) + "..."
}
