package skills

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxIndexRunes caps the whole rendered skills block so a large library
// cannot blow the system prompt. Selection stays complete by name; only
// descriptions give way first.
const maxIndexRunes = 6000

// RenderIndex renders the load-bearing selection block: every visible
// skill as "category: name: description" rows inside an <available_skills>
// envelope, plus the load rule. It returns "" when there is nothing to
// list so callers can omit the block entirely.
func RenderIndex(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	byCategory := make(map[string][]Skill, len(skills))
	for _, skill := range skills {
		byCategory[skill.Category] = append(byCategory[skill.Category], skill)
	}
	categories := make([]string, 0, len(byCategory))
	for category := range byCategory {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	var rows []string
	for _, category := range categories {
		members := byCategory[category]
		sort.Slice(members, func(i, j int) bool { return members[i].LoadName < members[j].LoadName })
		if len(members) == 1 && members[0].Category == "general" {
			rows = append(rows, skillRow(members[0]))
			continue
		}
		rows = append(rows, "  "+category+":")
		for _, skill := range members {
			rows = append(rows, "    "+skillRow(skill))
		}
	}
	block := "## Skills\n" +
		"Before replying, scan the skills below. If a skill matches or is even partially relevant to your task, " +
		"load it with skill_manage(action=\"view\", name=\"<skill>\") and follow its instructions. " +
		"Err on the side of loading — it is always better to have context you don't need than to miss established workflows. " +
		"Skills also encode the user's preferred approach for tasks like code review, planning, and testing — load them even for tasks you already know how to do.\n" +
		"If a skill has issues, fix it with skill_manage(action=\"update\").\n" +
		"After difficult/iterative tasks, offer to save the approach as a skill. If a skill you loaded was missing steps, " +
		"had wrong commands, or needed pitfalls you discovered, update it before finishing.\n" +
		"Only proceed without loading a skill if genuinely none are relevant to the task.\n" +
		"<available_skills>\n" + strings.Join(rows, "\n") + "\n</available_skills>"
	return clipIndex(block)
}

// skillRow renders one "- load_name: description" row.
func skillRow(skill Skill) string {
	if skill.Description == "" {
		return fmt.Sprintf("- %s", skill.LoadName)
	}
	return fmt.Sprintf("- %s: %s", skill.LoadName, skill.Description)
}

// clipIndex trims whole rows from the tail when the block exceeds its
// budget, keeping the envelope and load rule intact.
func clipIndex(block string) string {
	if utf8.RuneCountInString(block) <= maxIndexRunes {
		return block
	}
	lines := strings.Split(block, "\n")
	head := 0
	for head < len(lines) && lines[head] != "<available_skills>" {
		head++
	}
	kept := lines[:head+1]
	budget := maxIndexRunes - utf8.RuneCountInString(strings.Join(append(append([]string{}, kept...), "</available_skills>"), "\n"))
	rows := 0
	for _, line := range lines[head+1:] {
		if line == "</available_skills>" {
			break
		}
		if budget-utf8.RuneCountInString(line)-1 < 0 {
			break
		}
		budget -= utf8.RuneCountInString(line) + 1
		kept = append(kept, line)
		rows++
	}
	kept = append(kept, fmt.Sprintf("</available_skills>\n(%d skills listed; use skill_manage(action=\"list\") to see them all.)", rows))
	return strings.Join(kept, "\n")
}
