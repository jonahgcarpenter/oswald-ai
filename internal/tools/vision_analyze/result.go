package vision_analyze

import (
	"fmt"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

func resultText(question string, geometry *llm.ImageGeometry, firstFrame bool) string {
	text := "Image loaded into your context—you can see it natively now.\nUse your built-in vision to answer the user.\n\nQuestion: " + question
	if geometry != nil {
		text += fmt.Sprintf("\n\nSource image: %d×%d.", geometry.SourceWidth, geometry.SourceHeight)
	}
	if firstFrame {
		text += "\n\nNote: Showing only the first frame of this GIF."
	}
	return text + media.ImageGeometryNote(geometry)
}
