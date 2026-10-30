package skills

import (
	"strings"
	"unicode/utf8"
)

// Bounds for selection metadata. Bodies have their own cap in read.go.
const (
	// maxDescriptionRunes clips one skill's index description.
	maxDescriptionRunes = 200
	// maxSkillNameRunes bounds a declared skill name.
	maxSkillNameRunes = 64
	// maxSkillFileBytes caps how much of one SKILL.md is ever read.
	maxSkillFileBytes = 1 << 20
)

// parsedSkill is the frontmatter subset the catalog understands. Unknown
// keys are ignored so libraries carrying richer metadata keep working.
type parsedSkill struct {
	name          string
	description   string
	version       string
	platforms     []string
	prerequisites []string
	tags          []string
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

// parseFrontmatterKeys reads flat "key: value" pairs plus "- item" list
// continuations. Values may be bare, single/double quoted, or [a, b]
// inline lists; full-line "#" comments are dropped.
func parseFrontmatterKeys(lines []string) parsedSkill {
	var out parsedSkill
	current := ""
	flush := func(key, value string) {
		value = unquote(strings.TrimSpace(cutComment(value)))
		switch key {
		case "name":
			out.name = value
		case "description":
			out.description = value
		case "version":
			out.version = value
		case "platforms":
			out.platforms = append(out.platforms, splitListValue(value)...)
		case "prerequisites":
			out.prerequisites = append(out.prerequisites, splitListValue(value)...)
		case "tags":
			out.tags = append(out.tags, splitListValue(value)...)
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && current != "" {
			flush(current, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			current = ""
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		switch key {
		case "name", "description", "version", "platforms", "prerequisites", "tags":
			current = key
			flush(key, value)
		default:
			current = ""
		}
	}
	return out
}

// splitListValue expands "a, b" and "[a, b]" inline forms into items.
func splitListValue(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = unquote(strings.TrimSpace(item)); item != "" {
			items = append(items, item)
		}
	}
	return items
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
