package media

import (
	"fmt"
	"math"
	"strings"

	"github.com/jonahgcarpenter/oswald-ai/internal/llm"
)

// RefreshImageGeometryNote updates the server-rendered geometry suffix to match
// the image dimensions actually sent on a model attempt.
func RefreshImageGeometryNote(content string, previous, current *llm.ImageGeometry) string {
	return strings.TrimSuffix(content, ImageGeometryNote(previous)) + ImageGeometryNote(current)
}

// ImageGeometryNote describes cropping/downscaling and maps displayed coordinates
// back to the source image. Unchanged full images need no note.
func ImageGeometryNote(geometry *llm.ImageGeometry) string {
	if geometry == nil || geometry.Width <= 0 || geometry.Height <= 0 {
		return ""
	}
	width, height := geometry.SourceWidth, geometry.SourceHeight
	x, y := 0, 0
	if geometry.Region != nil {
		r := *geometry.Region
		x, y, width, height = r[0], r[1], r[2]-r[0], r[3]-r[1]
	}
	if width <= 0 || height <= 0 {
		return ""
	}
	resized := width != geometry.Width || height != geometry.Height
	if !resized && geometry.Region == nil {
		return ""
	}
	var text strings.Builder
	text.WriteString("\n\nNote: ")
	if geometry.Region != nil {
		r := *geometry.Region
		fmt.Fprintf(&text, "Showing source region [%d, %d, %d, %d].", r[0], r[1], r[2], r[3])
		if resized {
			fmt.Fprintf(&text, "\nCrop downscaled from %d×%d to %d×%d for vision.", width, height, geometry.Width, geometry.Height)
		}
	} else {
		fmt.Fprintf(&text, "Image downscaled from %d×%d to %d×%d for vision.", width, height, geometry.Width, geometry.Height)
	}
	sx, sy := float64(width)/float64(geometry.Width), float64(height)/float64(geometry.Height)
	if geometry.Region == nil && math.Abs(sx-sy) < 1e-9 {
		fmt.Fprintf(&text, "\nMultiply displayed coordinates by %s to map back to the source image.", formatCoordinateScale(sx))
	} else {
		fmt.Fprintf(&text, "\nMap displayed coordinates back to the source image:\nx = displayed x × %s + %d\ny = displayed y × %s + %d", formatCoordinateScale(sx), x, formatCoordinateScale(sy), y)
	}
	return text.String()
}

func formatCoordinateScale(scale float64) string {
	text := strings.TrimRight(fmt.Sprintf("%.6f", scale), "0")
	if strings.HasSuffix(text, ".") {
		return text + "00"
	}
	if len(text)-strings.IndexByte(text, '.') == 2 {
		text += "0"
	}
	return text
}
