package media

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestNormalizeInputImageDoesNotResizeSmallJPEG(t *testing.T) {
	raw := encodeTestJPEG(t, 640, 480)

	result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "small.jpg")
	if err != nil {
		t.Fatalf("normalize small jpeg: %v", err)
	}

	if result.WasResized {
		t.Fatal("expected small image not to resize")
	}
	if result.OriginalWidth != 640 || result.OriginalHeight != 480 || result.Width != 640 || result.Height != 480 {
		t.Fatalf("unexpected dimensions: original=%dx%d normalized=%dx%d", result.OriginalWidth, result.OriginalHeight, result.Width, result.Height)
	}
	if result.Image.MimeType != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", result.Image.MimeType)
	}
	if result.NormalizedBytes == 0 || result.Base64Chars != len(result.Image.Data) {
		t.Fatalf("unexpected encoded sizes: bytes=%d base64=%d data=%d", result.NormalizedBytes, result.Base64Chars, len(result.Image.Data))
	}
}

func TestNormalizeInputImageResizesLargeLandscapeJPEG(t *testing.T) {
	raw := encodeTestJPEG(t, 4032, 3024)

	result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "large.jpg")
	if err != nil {
		t.Fatalf("normalize large jpeg: %v", err)
	}

	if !result.WasResized {
		t.Fatal("expected large image to resize")
	}
	if result.OriginalWidth != 4032 || result.OriginalHeight != 3024 {
		t.Fatalf("unexpected original dimensions: %dx%d", result.OriginalWidth, result.OriginalHeight)
	}
	if result.Width != MaxNormalizedImageLongEdge || result.Height != 1920 {
		t.Fatalf("unexpected normalized dimensions: %dx%d", result.Width, result.Height)
	}
	if result.Image.MimeType != "image/jpeg" {
		t.Fatalf("expected image/jpeg, got %q", result.Image.MimeType)
	}
}

func TestNormalizeInputImageResizesLargePortraitJPEG(t *testing.T) {
	raw := encodeTestJPEG(t, 3024, 4032)

	result, err := NormalizeInputImageFromBytes(nil, "image/jpeg", raw, "portrait.jpg")
	if err != nil {
		t.Fatalf("normalize portrait jpeg: %v", err)
	}

	if !result.WasResized {
		t.Fatal("expected portrait image to resize")
	}
	if result.Width != 1920 || result.Height != MaxNormalizedImageLongEdge {
		t.Fatalf("unexpected normalized dimensions: %dx%d", result.Width, result.Height)
	}
}

func TestNormalizeInputImagePreservesTransparentPNG(t *testing.T) {
	raw := encodeTestPNG(t, 3000, 2000, true)

	result, err := NormalizeInputImageFromBytes(nil, "image/png", raw, "transparent.png")
	if err != nil {
		t.Fatalf("normalize transparent png: %v", err)
	}

	if !result.WasResized {
		t.Fatal("expected transparent png to resize")
	}
	if !result.PreservedAlpha {
		t.Fatal("expected alpha to be preserved")
	}
	if result.Image.MimeType != "image/png" {
		t.Fatalf("expected image/png, got %q", result.Image.MimeType)
	}
	if result.Width != MaxNormalizedImageLongEdge || result.Height != 1707 {
		t.Fatalf("unexpected normalized dimensions: %dx%d", result.Width, result.Height)
	}
	if _, err := base64.StdEncoding.DecodeString(result.Image.Data); err != nil {
		t.Fatalf("normalized payload is not valid base64: %v", err)
	}
}

func encodeTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fillTestImage(img, false)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

func encodeTestPNG(t *testing.T, width, height int, transparent bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fillTestImage(img, transparent)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func fillTestImage(img *image.RGBA, transparent bool) {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			alpha := uint8(255)
			if transparent && (x+y)%17 == 0 {
				alpha = 96
			}
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: alpha})
		}
	}
}
