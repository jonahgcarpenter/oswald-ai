package skills

import (
	"strings"
	"testing"
)

func TestRenderIndexEmpty(t *testing.T) {
	if got := RenderIndex(nil); got != "" {
		t.Fatalf("expected empty index, got %q", got)
	}
}

func TestRenderIndexRows(t *testing.T) {
	skills := []Skill{
		{Name: "review", Description: "Review code", Category: "code", LoadName: "review"},
		{Name: "lonely", Description: "", Category: "general", LoadName: "lonely"},
	}
	block := RenderIndex(skills)
	for _, want := range []string{"## Skills", "<available_skills>", "- review: Review code", "- lonely", "</available_skills>", "skill_manage"} {
		if !strings.Contains(block, want) {
			t.Fatalf("expected %q in index:\n%s", want, block)
		}
	}
	for _, want := range []string{
		`fix it with skill_manage(action="update")`,
		"offer to save the approach as a skill",
		"update it before finishing",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("expected coaching %q in index:\n%s", want, block)
		}
	}
}

func TestRenderIndexCapsLargeLibraries(t *testing.T) {
	var skills []Skill
	for i := 0; i < 500; i++ {
		skills = append(skills, Skill{Name: "s", Description: strings.Repeat("d", maxDescriptionRunes), Category: "general", LoadName: "skill"})
	}
	block := RenderIndex(skills)
	if got := len([]rune(block)); got > maxIndexRunes+200 {
		t.Fatalf("index exceeds budget: %d runes", got)
	}
	if !strings.Contains(block, "</available_skills>") {
		t.Fatal("expected intact envelope after clipping")
	}
}
