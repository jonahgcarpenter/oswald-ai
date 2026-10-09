package image_generate

import (
	"strings"
	"testing"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func TestResultTextIncludesPathAndApplicablePreviewNotes(t *testing.T) {
	geometry := &llm.ImageGeometry{SourceWidth: 1280, SourceHeight: 720, Width: 1280, Height: 720}
	text := ResultText("/private/generated.png", geometry, false)
	for _, want := range []string{"Image generated successfully", "latest successful version", "Image: /private/generated.png", "Source image: 1280×720."} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in generated result: %q", want, text)
		}
	}
	if strings.Contains(text, "Note:") {
		t.Fatal("unchanged preview has a needless note")
	}
	resized := *geometry
	resized.Width, resized.Height = 960, 540
	updated := media.RefreshImageGeometryNote(text, geometry, &resized)
	if !strings.Contains(updated, "from 1280×720 to 960×540") || !strings.Contains(updated, "by 1.333333") || !strings.Contains(updated, "Image: /private/generated.png") {
		t.Fatal("resize note lost path or used incorrect geometry")
	}
	gif := ResultText("/private/generated.gif", geometry, true)
	if !strings.Contains(gif, "Showing only the first frame") || !strings.Contains(gif, "original GIF will be delivered") {
		t.Fatal("GIF preview did not explain first-frame loading and original delivery")
	}
}
