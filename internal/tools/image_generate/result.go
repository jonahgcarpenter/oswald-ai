package image_generate

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

// ResultText describes the cached output and request-local model preview.
// Generated images are never delivered automatically; the model delivers one
// by including its MEDIA:/absolute/path token in the final response.
func ResultText(path string, geometry *llm.ImageGeometry, firstFrame bool) string {
	text := "Image generated successfully—you can see it natively now.\n\nImage: " + path
	if geometry != nil {
		text += fmt.Sprintf("\nSource image: %d×%d.", geometry.SourceWidth, geometry.SourceHeight)
	}
	if firstFrame {
		text += "\n\nNote: Showing only the first frame of this GIF; include its MEDIA token in your final response to deliver the original GIF."
	}
	return text + media.ImageGeometryNote(geometry)
}
