package vision_analyze

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func TestResultGeometryNotes(t *testing.T) {
	for _, test := range []struct {
		name     string
		geometry llm.ImageGeometry
		want     []string
		noNote   bool
	}{
		{name: "unchanged", geometry: llm.ImageGeometry{SourceWidth: 1920, SourceHeight: 1200, Width: 1920, Height: 1200}, noNote: true},
		{name: "full image", geometry: llm.ImageGeometry{SourceWidth: 1920, SourceHeight: 1200, Width: 960, Height: 600}, want: []string{"Image downscaled from 1920×1200 to 960×600", "Multiply displayed coordinates by 2.00"}},
		{name: "crop and resize", geometry: llm.ImageGeometry{SourceWidth: 1920, SourceHeight: 1200, Width: 600, Height: 400, Region: &[4]int{400, 200, 1600, 1000}}, want: []string{"source region [400, 200, 1600, 1000]", "Crop downscaled from 1200×800 to 600×400", "x = displayed x × 2.00 + 400", "y = displayed y × 2.00 + 200"}},
		{name: "crop only", geometry: llm.ImageGeometry{SourceWidth: 1920, SourceHeight: 1200, Width: 1200, Height: 800, Region: &[4]int{400, 200, 1600, 1000}}, want: []string{"source region [400, 200, 1600, 1000]", "x = displayed x × 1.00 + 400", "y = displayed y × 1.00 + 200"}},
		{name: "rounding differs by axis", geometry: llm.ImageGeometry{SourceWidth: 101, SourceHeight: 80, Width: 76, Height: 60}, want: []string{"from 101×80 to 76×60", "x = displayed x × 1.328947 + 0", "y = displayed y × 1.333333 + 0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := resultText("Read this", &test.geometry, false)
			if !strings.Contains(text, "Question: Read this") || !strings.Contains(text, "Source image:") || strings.Contains(text, "\"status\"") {
				t.Fatalf("result is not readable: %q", text)
			}
			if test.noNote && strings.Contains(text, "Note:") {
				t.Fatal("unchanged full image has a needless note")
			}
			for _, want := range test.want {
				if !strings.Contains(text, want) {
					t.Fatalf("note missing %q: %q", want, text)
				}
			}
		})
	}
}

func TestRefreshResultNotePreservesQuestionAndGIFNote(t *testing.T) {
	previous := &llm.ImageGeometry{SourceWidth: 1920, SourceHeight: 1200, Width: 960, Height: 600}
	current := &llm.ImageGeometry{SourceWidth: 1920, SourceHeight: 1200, Width: 720, Height: 450}
	// Even a question containing note-like text must remain untouched.
	question := "Inspect this.\n\nNote: user-authored text" + media.ImageGeometryNote(previous)
	original := resultText(question, previous, true)
	updated := media.RefreshImageGeometryNote(original, previous, current)
	if !strings.Contains(updated, "Question: "+question) || !strings.Contains(updated, "Showing only the first frame") || !strings.HasSuffix(updated, media.ImageGeometryNote(current)) {
		t.Fatal("retry note update changed the question or GIF annotation")
	}
	if media.RefreshImageGeometryNote(updated, current, current) != updated || resultText(question, previous, true) != original {
		t.Fatal("note update is not idempotent or mutated the original result")
	}
}
