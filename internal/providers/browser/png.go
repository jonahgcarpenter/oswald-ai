package browser

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// encodePNG encodes an image as PNG.
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// strokeRect draws a 2px border.
func strokeRect(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	b := img.Bounds()
	for x := x0; x <= x1; x++ {
		for _, y := range []int{y0, y0 + 1, y1 - 1, y1} {
			if (image.Point{X: x, Y: y}).In(b) {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	for y := y0; y <= y1; y++ {
		for _, x := range []int{x0, x0 + 1, x1 - 1, x1} {
			if (image.Point{X: x, Y: y}).In(b) {
				img.SetNRGBA(x, y, c)
			}
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
