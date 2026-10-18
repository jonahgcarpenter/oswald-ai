package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"
)

func TestVisionCropBeforeDownscaling(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3000, 30))
	img.SetNRGBA(2800, 10, color.NRGBA{R: 255, A: 255})
	var source bytes.Buffer
	if err := png.Encode(&source, img); err != nil {
		t.Fatal(err)
	}
	region := image.Rect(2790, 0, 2820, 30)
	result, err := NormalizeVisionImage(context.Background(), source.Bytes(), "image/png", &region)
	if err != nil {
		t.Fatal(err)
	}
	if result.OriginalWidth != 3000 || result.Width != 30 || result.Height != 30 || result.WasResized {
		t.Fatalf("unexpected dimensions: %+v", result)
	}
	data, err := base64.StdEncoding.DecodeString(result.Image.Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if color.NRGBAModel.Convert(decoded.At(10, 10)).(color.NRGBA) != (color.NRGBA{R: 255, A: 255}) {
		t.Fatal("crop lost source-resolution pixel")
	}
	full, err := NormalizeVisionImage(context.Background(), source.Bytes(), "image/png", nil)
	if err != nil || full.Width > MaxNormalizedImageLongEdge || full.NormalizedBytes > MaxNormalizedImageBytes || !full.WasResized {
		t.Fatalf("full image not bounded: err=%v", err)
	}
	for _, invalid := range []image.Rectangle{image.Rect(0, 0, 3001, 30), image.Rect(-1, 0, 10, 10), {}} {
		if _, err := NormalizeVisionImage(context.Background(), source.Bytes(), "image/png", &invalid); err == nil {
			t.Fatal("accepted invalid crop")
		}
	}
}

func TestVisionGIFLoadsOnlyFirstFrame(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	second := image.NewPaletted(first.Bounds(), palette)
	second.SetColorIndex(0, 0, 1)
	var source bytes.Buffer
	if err := gif.EncodeAll(&source, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	result, err := NormalizeVisionImage(context.Background(), source.Bytes(), "image/gif", nil)
	if err != nil || result.Width != 3 || result.Height != 2 || result.DecodedFormat != "gif" || result.Image.IsGIFContactSheet {
		t.Fatalf("GIF not treated as a still frame: err=%v", err)
	}
}

func TestVisionRejectsOversizedDimensions(t *testing.T) {
	var source bytes.Buffer
	if err := png.Encode(&source, image.NewGray(image.Rect(0, 0, 8193, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeVisionImage(context.Background(), source.Bytes(), "image/png", nil); err == nil {
		t.Fatal("accepted oversized dimension")
	}
	if _, err := NormalizeVisionImage(context.Background(), []byte("not an image"), "image/png", nil); err == nil {
		t.Fatal("accepted malformed image")
	}
}
