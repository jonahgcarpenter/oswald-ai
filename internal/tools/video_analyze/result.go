package video_analyze

import (
	"fmt"
	"strings"
)

// resultText renders the tool result: what was sampled, per-frame cache
// paths for vision_analyze drill-down, and the explicit perception limits.
// Timestamps and paths ride the text so the answer stays searchable after
// the image bytes leave context.
func resultText(question string, stamps []float64, duration float64, sheets int, frames []videoFrame) string {
	var b strings.Builder
	count := len(stamps)
	frameWord := "still frames"
	if count == 1 {
		frameWord = "still frame"
	}
	listed := make([]string, 0, count)
	for _, stamp := range stamps {
		listed = append(listed, fmt.Sprintf("%.1fs", stamp))
	}
	fmt.Fprintf(&b, "Video loaded into your context as %d %s (t=%s of %.1fs total)—you can see them natively now.\nUse your built-in vision to answer the user.\n\nQuestion: %s", count, frameWord, strings.Join(listed, ", "), duration, question)
	if count == 1 && len(frames) == 1 {
		geometry := ""
		if frames[0].geometry != "" {
			geometry = ": " + frames[0].geometry
		}
		fmt.Fprintf(&b, "\n\nFrame 1/1 @ %.1fs%s → %s.", stamps[0], geometry, frames[0].path)
	} else {
		labels := []string{"top-left", "top-right", "bottom-left", "bottom-right"}
		for s := 0; s < sheets; s++ {
			parts := make([]string, 0, 4)
			for i := 0; i < 4 && s*4+i < count; i++ {
				frame := frames[s*4+i]
				parts = append(parts, fmt.Sprintf("%s t=%.1fs → %s", labels[i], stamps[s*4+i], frame.path))
			}
			fmt.Fprintf(&b, "\n\nSheet %d/%d: %s.", s+1, sheets, strings.Join(parts, ", "))
		}
	}
	b.WriteString("\n\nIndividual frames are cached for 24 hours—call vision_analyze with a path above to see one at full resolution.")
	b.WriteString("\n\nNote: motion between frames and the audio track were not perceived—answer only from what the frames show.")
	return b.String()
}
