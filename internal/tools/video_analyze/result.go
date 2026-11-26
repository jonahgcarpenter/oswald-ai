package video_analyze

import (
	"fmt"
	"strings"
)

// resultText renders the tool result: what was sampled, per-frame geometry,
// and the explicit perception limits. Timestamps ride the text so the answer
// stays searchable after the image bytes leave context.
func resultText(question string, stamps []float64, duration float64, sheets int, geometries []string) string {
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
	if sheets <= 1 {
		for i, stamp := range stamps {
			geometry := ""
			if i < len(geometries) && geometries[i] != "" {
				geometry = ": " + geometries[i]
			}
			fmt.Fprintf(&b, "\n\nFrame %d/%d @ %.1fs%s.", i+1, count, stamp, geometry)
		}
	} else {
		labels := []string{"top-left", "top-right", "bottom-left", "bottom-right"}
		for s := 0; s < sheets; s++ {
			parts := make([]string, 0, 4)
			for i := 0; i < 4 && s*4+i < count; i++ {
				parts = append(parts, fmt.Sprintf("%s t=%.1fs", labels[i], stamps[s*4+i]))
			}
			fmt.Fprintf(&b, "\n\nSheet %d/%d: %s.", s+1, sheets, strings.Join(parts, ", "))
		}
	}
	b.WriteString("\n\nNote: motion between frames and the audio track were not perceived—answer only from what the frames show.")
	return b.String()
}
