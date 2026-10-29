package image_generate

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/providers/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/transport/media"
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
	// The geometry note must stay the string suffix for
	// media.RefreshImageGeometryNote, so the attach hint precedes it.
	text += "\n\nTo attach to your response use:\nMEDIA: " + path
	return text + media.ImageGeometryNote(geometry)
}
